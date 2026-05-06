//go:build !windows

package audit

import (
	"encoding/json"
	"fmt"
	"log/syslog"
)

// SyslogSink sends each event as a JSON string to the local syslog daemon
// at LOG_INFO|LOG_DAEMON priority under the "muninn-audit" tag.
type SyslogSink struct {
	w *syslog.Writer
}

// NewSyslogSink opens a connection to the local syslog daemon.
func NewSyslogSink() (*SyslogSink, error) {
	w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "muninn-audit")
	if err != nil {
		return nil, fmt.Errorf("audit: syslog: %w", err)
	}
	return &SyslogSink{w: w}, nil
}

func (s *SyslogSink) Write(e AuditEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.w.Info(string(b))
}

func (s *SyslogSink) Close() error { return s.w.Close() }
