package audit

import (
	"os"
	"path/filepath"
	"strconv"
)

// ServerConfig controls which sinks are active and their parameters.
type ServerConfig struct {
	// FilePath is the NDJSON file sink path.
	// Empty string disables the file sink. Default: <datadir>/audit.log
	FilePath string

	// Stdout enables writing every event to stdout.
	Stdout bool

	// Syslog enables writing every event to the local syslog daemon.
	Syslog bool

	// WebhookURL, if non-empty, enables the HTTP webhook sink.
	WebhookURL string

	// BufferSize is the async channel depth. Default: 4096.
	BufferSize int
}

// ConfigFromEnv reads audit configuration from environment variables.
// dataDir is used to compute the default file path.
//
// Env vars:
//
//	MUNINN_AUDIT_FILE       — file path (default: <datadir>/audit.log; "-" disables)
//	MUNINN_AUDIT_STDOUT     — "1" or "true" enables stdout sink
//	MUNINN_AUDIT_SYSLOG     — "1" or "true" enables syslog sink
//	MUNINN_AUDIT_WEBHOOK_URL — HTTP endpoint for webhook sink
//	MUNINN_AUDIT_BUFFER     — async channel size (default: 4096)
func ConfigFromEnv(dataDir string) ServerConfig {
	cfg := ServerConfig{
		FilePath:   filepath.Join(dataDir, "audit.log"),
		BufferSize: 4096,
	}

	if v := os.Getenv("MUNINN_AUDIT_FILE"); v != "" {
		if v == "-" {
			cfg.FilePath = ""
		} else {
			cfg.FilePath = v
		}
	}
	cfg.Stdout = envBool("MUNINN_AUDIT_STDOUT")
	cfg.Syslog = envBool("MUNINN_AUDIT_SYSLOG")
	cfg.WebhookURL = os.Getenv("MUNINN_AUDIT_WEBHOOK_URL")

	if v := os.Getenv("MUNINN_AUDIT_BUFFER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BufferSize = n
		}
	}
	return cfg
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true"
}
