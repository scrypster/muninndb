package mbp

// ErrorCode represents standardized error codes for the protocol.
type ErrorCode uint16

// Error code constants.
const (
	ErrOK                    ErrorCode = 1000
	ErrEngramNotFound        ErrorCode = 4001
	ErrVaultNotFound         ErrorCode = 4002
	ErrInvalidEngram         ErrorCode = 4003
	ErrIdempotencyViolation  ErrorCode = 4004
	ErrInvalidAssociation    ErrorCode = 4005
	ErrSubscriptionNotFound  ErrorCode = 4006
	ErrThresholdInvalid      ErrorCode = 4007
	ErrHopDepthExceeded      ErrorCode = 4008
	ErrWeightsInvalid        ErrorCode = 4009
	ErrAuthFailed            ErrorCode = 4010
	ErrVaultForbidden        ErrorCode = 4011
	ErrRateLimited           ErrorCode = 4012
	ErrMaxResultsExceeded    ErrorCode = 4013
	ErrInvalidClusterRequest ErrorCode = 4014
	ErrStorageError          ErrorCode = 5001
	ErrIndexError            ErrorCode = 5002
	ErrEnrichmentError       ErrorCode = 5003
	ErrShardUnavailable      ErrorCode = 5004
	ErrInternal              ErrorCode = 5005
)

// ErrorPayload is the msgpack body of a TypeError frame.
type ErrorPayload struct {
	Code       ErrorCode `msgpack:"code"`
	Message    string    `msgpack:"message"`
	RequestID  string    `msgpack:"request_id,omitempty"`
	RetryAfter int       `msgpack:"retry_after,omitempty"`
}

// ErrorCodeMessage returns a human-readable message for an error code.
func ErrorCodeMessage(code ErrorCode) string {
	switch code {
	case ErrOK:
		return "OK"
	case ErrEngramNotFound:
		return "engram not found"
	case ErrVaultNotFound:
		return "vault not found"
	case ErrInvalidEngram:
		return "invalid engram"
	case ErrIdempotencyViolation:
		return "idempotency violation"
	case ErrInvalidAssociation:
		return "invalid association"
	case ErrSubscriptionNotFound:
		return "subscription not found"
	case ErrThresholdInvalid:
		return "threshold invalid"
	case ErrHopDepthExceeded:
		return "hop depth exceeded"
	case ErrWeightsInvalid:
		return "weights invalid"
	case ErrAuthFailed:
		return "auth failed"
	case ErrVaultForbidden:
		return "vault forbidden"
	case ErrRateLimited:
		return "rate limited"
	case ErrMaxResultsExceeded:
		return "max results exceeded"
	case ErrInvalidClusterRequest:
		return "invalid cluster request"
	case ErrStorageError:
		return "storage error"
	case ErrIndexError:
		return "index error"
	case ErrEnrichmentError:
		return "enrichment error"
	case ErrShardUnavailable:
		return "shard unavailable"
	case ErrInternal:
		return "internal error"
	default:
		return "unknown error"
	}
}
