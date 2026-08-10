package enrich

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// canary stands in for what model output actually is: a function of the memory that
// was enriched. If it reaches a log, so does the memory.
const canary = "PATIENT-4417-DIAGNOSIS-CONFIDENTIAL"

func TestParseError_MessageCarriesNoPayload(t *testing.T) {
	// Each parser fed a response that cannot be decoded into its shape.
	malformed := `{"entities": ["` + canary + `"], "relationships": ["` + canary +
		`"], "memory_type": {"nested": "` + canary + `"}, "summary": ["` + canary + `"]}`

	cases := []struct {
		name  string
		stage string
		call  func(string) error
	}{
		{"entities", "entities", func(s string) error { _, err := ParseEntityResponse(s); return err }},
		{"relationships", "relationships", func(s string) error { _, err := ParseRelationshipResponse(s); return err }},
		{"classification", "classification", func(s string) error {
			_, _, _, _, _, err := ParseClassificationResponse(s)
			return err
		}},
		{"summary", "summary", func(s string) error { _, _, err := ParseSummarizeResponse(s); return err }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(malformed)
			if err == nil {
				t.Fatal("expected a parse error")
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("model output leaked into the error: %s", err.Error())
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error is not a *ParseError: %T", err)
			}
			if pe.Stage != tc.stage {
				t.Errorf("stage = %q, want %q", pe.Stage, tc.stage)
			}
			if pe.Category == "" {
				t.Error("parse error carries no category")
			}
			if pe.Bytes != len(malformed) {
				t.Errorf("Bytes = %d, want %d", pe.Bytes, len(malformed))
			}
		})
	}
}

// json.UnmarshalTypeError renders its Value as descriptions like "number -5", which
// would put a value straight out of the response back into the message. Only Field
// is safe to carry, and this pins that.
func TestParseError_TypeMismatchNamesTheFieldNotTheValue(t *testing.T) {
	resp := `{"entities": [{"name": "x", "type": "y", "confidence": "` + canary + `"}]}`

	_, err := ParseEntityResponse(resp)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("the rejected value leaked into the error: %s", err.Error())
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("error is not a *ParseError: %T", err)
	}
	if pe.Category != ParseErrTypeMismatch {
		t.Errorf("category = %q, want %q", pe.Category, ParseErrTypeMismatch)
	}
	if pe.Field == "" {
		t.Error("a type mismatch should name the field it rejected")
	}
}

func TestParseError_EmptyResultIsItsOwnCategory(t *testing.T) {
	// Decodes cleanly, carries nothing usable — a different failure from malformed
	// output, and worth telling apart in a log.
	if _, _, _, _, _, err := ParseClassificationResponse(`{}`); err == nil {
		t.Error("expected an error for an empty classification")
	} else {
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Category != ParseErrEmptyResult {
			t.Errorf("classification: got %v, want category %q", err, ParseErrEmptyResult)
		}
	}
	if _, _, err := ParseSummarizeResponse(`{}`); err == nil {
		t.Error("expected an error for an empty summary")
	} else {
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Category != ParseErrEmptyResult {
			t.Errorf("summary: got %v, want category %q", err, ParseErrEmptyResult)
		}
	}
}

func TestParseError_SuccessfulParsingIsUnchanged(t *testing.T) {
	entities, err := ParseEntityResponse(`{"entities": [{"name": "PostgreSQL", "type": "database", "confidence": 0.95}]}`)
	if err != nil {
		t.Fatalf("valid entity response failed to parse: %v", err)
	}
	if len(entities) != 1 || entities[0].Name != "PostgreSQL" {
		t.Errorf("entities = %+v", entities)
	}

	summary, _, err := ParseSummarizeResponse(`{"summary": "A database."}`)
	if err != nil {
		t.Fatalf("valid summarize response failed to parse: %v", err)
	}
	if summary != "A database." {
		t.Errorf("summary = %q", summary)
	}
}

// captureLogs installs a buffer-backed default logger at debug level and returns the
// buffer plus a restore func.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// The behaviour the issue asks for, at the level it actually happens: Run's per-stage
// warnings are emitted at warn level, not only under verbose, so an unredacted parse
// error reaches a default-configured server's logs.
func TestPipelineRun_DoesNotLogModelOutput(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		name := "default_logs"
		if verbose {
			name = "verbose_logs"
		}
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)

			mock := NewMockLLMProvider()
			mock.customComplete = func(context.Context, string, string) (string, error) {
				return `{"entities": ["` + canary + `"]}`, nil
			}
			pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
			v := verbose
			pipeline.SetConfig(&config.PluginConfig{LLMVerboseLogs: &v})

			_, err := pipeline.Run(context.Background(), &storage.Engram{})
			if err == nil {
				t.Fatal("expected the pipeline to fail on unparseable output")
			}

			logs := buf.String()
			if strings.Contains(logs, canary) {
				t.Errorf("model output reached the logs:\n%s", logs)
			}
			// The error the pipeline hands back is logged by callers too.
			if strings.Contains(err.Error(), canary) {
				t.Errorf("model output reached the returned error: %v", err)
			}
			// Redaction must not cost observability.
			if !strings.Contains(logs, "entities") {
				t.Errorf("logs no longer identify the failing stage:\n%s", logs)
			}
			if !strings.Contains(logs, string(ParseErrUnmarshal)) &&
				!strings.Contains(logs, string(ParseErrTypeMismatch)) {
				t.Errorf("logs carry no error category:\n%s", logs)
			}
		})
	}
}

// #640/#643 draw the line between a transient provider failure and permanently
// malformed output. Retyping parse errors must not move it.
func TestParseError_RemainsPermanentNotRetryable(t *testing.T) {
	_, err := ParseEntityResponse(`not-json`)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	var provErr *plugin.ProviderError
	if errors.As(err, &provErr) {
		t.Fatalf("a parse error must not satisfy *plugin.ProviderError: %+v", provErr)
	}
	if shouldAbortPipeline(err) {
		t.Error("a parse error must not abort the pipeline — other stages still run")
	}
}
