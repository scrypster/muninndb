package enrich

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ParseErrorCategory is a stable, payload-free classification of why a model
// response could not be turned into structured output. The values are part of what
// gets logged, so they are meant to be greppable and to stay put.
type ParseErrorCategory string

const (
	// ParseErrUnmarshal — the response was not decodable into the expected shape.
	ParseErrUnmarshal ParseErrorCategory = "unmarshal_failure"
	// ParseErrTypeMismatch — decodable, but a field held the wrong JSON type.
	ParseErrTypeMismatch ParseErrorCategory = "type_mismatch"
	// ParseErrEmptyResult — decoded cleanly, but every field we need was empty.
	ParseErrEmptyResult ParseErrorCategory = "empty_result"
)

// ParseError reports a failed parse of a model response with enough detail to
// diagnose it and nothing derived from the response itself.
//
// The response is *not* carried, not even truncated. Model output is a function of
// memory content, so a fragment of it in a log is a fragment of a user's memory in a
// log — and these errors reach the log at warn level, not only under verbose. The
// transport layer already holds this line (`providerHTTPError` drains the provider's
// body specifically so it is never retained or logged); this is the parse layer
// holding the same one.
type ParseError struct {
	// Stage is the pipeline call that failed: entities, relationships,
	// classification, or summary.
	Stage string
	// Category is the payload-free reason.
	Category ParseErrorCategory
	// Field is the struct field the decoder rejected, when it named one. It comes
	// from our own struct tags, never from the response.
	Field string
	// Bytes is the size of the response we failed on — enough to tell an empty reply
	// from a wall of prose without reproducing either.
	Bytes int
}

func (e *ParseError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s at %q (%d bytes of model output)",
			e.Stage, e.Category, e.Field, e.Bytes)
	}
	return fmt.Sprintf("%s: %s (%d bytes of model output)", e.Stage, e.Category, e.Bytes)
}

// newParseError classifies a decode failure without reading the payload.
//
// It reads *json.UnmarshalTypeError.Field, which is our schema, but deliberately not
// its Value: the standard library renders that as descriptions like "number -5",
// which would put a value from the response straight back into the message.
func newParseError(stage string, respBytes int, err error) *ParseError {
	pe := &ParseError{Stage: stage, Category: ParseErrUnmarshal, Bytes: respBytes}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		pe.Category = ParseErrTypeMismatch
		pe.Field = typeErr.Field
	}
	return pe
}

// emptyResultError reports a response that decoded but carried nothing usable.
func emptyResultError(stage string, respBytes int) *ParseError {
	return &ParseError{Stage: stage, Category: ParseErrEmptyResult, Bytes: respBytes}
}
