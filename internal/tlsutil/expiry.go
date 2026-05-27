// Package tlsutil holds small TLS helpers shared between the main server and
// the cluster-replication subsystem.
package tlsutil

import (
	"crypto/x509"
	"log/slog"
	"time"
)

// ExpiryWarnWindow is the threshold below which a certificate is considered
// "expiring soon". 30 days matches Let's Encrypt's renewal window and is the
// de-facto standard across operational tooling.
const ExpiryWarnWindow = 30 * 24 * time.Hour

// DaysRemaining converts a positive duration to whole days, rounded up so a
// duration of 23h59m reports 1 (not 0). Returns 0 for non-positive input. The
// ceiling bias matches operator expectations: "you have N days to act."
func DaysRemaining(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	day := 24 * time.Hour
	return int((d + day - 1) / day)
}

// CheckCertExpiry inspects cert.NotAfter and logs at the appropriate severity:
//
//   - Error when the certificate is already expired. The caller decides whether
//     to abort; existing TLS connections may still function until the cert is
//     actually rejected by a peer, so failing loud (not shut) is intentional.
//   - Warn  when expiry is within ExpiryWarnWindow.
//   - Silent otherwise; the caller may attach expiry fields to its own Info
//     line so an operator can always see them at startup.
//
// The duration until NotAfter is returned (negative if already expired) so the
// caller can include it in its own logging without re-computing.
func CheckCertExpiry(logger *slog.Logger, cert *x509.Certificate, label string) time.Duration {
	if logger == nil {
		logger = slog.Default()
	}
	remaining := time.Until(cert.NotAfter)
	attrs := []any{
		"label", label,
		"subject", cert.Subject.CommonName,
		"not_after", cert.NotAfter.UTC().Format(time.RFC3339),
	}
	switch {
	case remaining <= 0:
		logger.Error("tls: certificate is expired",
			append(attrs, "expired_ago", (-remaining).Truncate(time.Hour).String())...)
	case remaining < ExpiryWarnWindow:
		logger.Warn("tls: certificate expires soon",
			append(attrs, "days_remaining", DaysRemaining(remaining))...)
	}
	return remaining
}
