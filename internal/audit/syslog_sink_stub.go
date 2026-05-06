//go:build windows

package audit

import "errors"

// SyslogSink is not available on Windows.
type SyslogSink struct{}

func NewSyslogSink() (*SyslogSink, error) {
	return nil, errors.New("audit: syslog sink not supported on Windows")
}
func (s *SyslogSink) Write(AuditEvent) error { return nil }
func (s *SyslogSink) Close() error           { return nil }
