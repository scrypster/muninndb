package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Query ids for the label pipeline (slice 2a).
//
// A normal (non-read-only) recall's query id is the ULID of its persisted
// 0x29 event. A read-only recall writes nothing, so its id must verify itself:
//
//	ro1.<ULID 26>.<rc 16 hex>.<rs 16 hex>.<mac 22 base64url>
//	mac = HMAC-SHA256(k_q, "ro1" 0x00 vault 0x00 ULID(16) rc(8) rs(8))[:16]
//	k_q = HMAC-SHA256(auth_secret, "muninn/query-id/v1")
//
// k_q is derived from the PERSISTED auth_secret (Bootstrap reads it at every
// boot), so an id minted before a restart verifies after it. The derivation
// label separates k_q from the admin-session signing use of the same secret.
//
// The MAC binds the vault, so an id replayed against another vault fails by
// construction. It is deliberately NOT bound to the labelling credential: a
// harness drain labels later, from another session and possibly another key.
// rc (the recall credential hash) is authenticated inside the id instead, so
// every label records whether its labeller was the recaller.

const (
	queryIDKeyLabel = "muninn/query-id/v1"
	roPrefixV1      = "ro1."
	roPrefixNoKey   = "ro0."
	ulidLen         = 26
	roHashHexLen    = 16
	roMACLen        = 22 // base64url, no padding, of 16 bytes
	// ROQueryIDLenV1 is the exact length of an ro1. id.
	ROQueryIDLenV1 = len(roPrefixV1) + ulidLen + 1 + roHashHexLen + 1 + roHashHexLen + 1 + roMACLen
)

// QueryIDKey is k_q. The zero value / nil means "no key wired" (an embedded
// library with no auth_secret): read-only ids are minted as ro0.<ULID> and
// labels against them are rejected.
type QueryIDKey struct {
	k []byte
}

// NewQueryIDKey derives k_q from the persisted auth secret. Returns nil for an
// empty secret.
func NewQueryIDKey(authSecret []byte) *QueryIDKey {
	if len(authSecret) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, authSecret)
	m.Write([]byte(queryIDKeyLabel))
	return &QueryIDKey{k: m.Sum(nil)}
}

func (q *QueryIDKey) sum8(tag, v string) [8]byte {
	var out [8]byte
	m := hmac.New(sha256.New, q.k)
	m.Write([]byte(tag))
	m.Write([]byte{0})
	m.Write([]byte(v))
	copy(out[:], m.Sum(nil))
	return out
}

// CredHash is the 8-byte keyed hash of a credential token. The open-server
// and static-token deployments hash the empty / static token, so every
// caller of such a server is one credential — stated, not hidden.
func (q *QueryIDKey) CredHash(token string) [8]byte {
	if q == nil {
		return [8]byte{}
	}
	return q.sum8("cred", token)
}

// SessionHash is the 8-byte keyed hash of an MCP session id; zeros when there
// is no session.
func (q *QueryIDKey) SessionHash(session string) [8]byte {
	if q == nil || session == "" {
		return [8]byte{}
	}
	return q.sum8("sess", session)
}

func (q *QueryIDKey) mac(vault string, id [16]byte, rc, rs [8]byte) []byte {
	m := hmac.New(sha256.New, q.k)
	m.Write([]byte("ro1"))
	m.Write([]byte{0})
	m.Write([]byte(vault))
	m.Write([]byte{0})
	m.Write(id[:])
	m.Write(rc[:])
	m.Write(rs[:])
	return m.Sum(nil)[:16]
}

// MintReadOnlyQueryID mints a read-only query id. idStr is the 26-char ULID
// string of id. With no key it returns ro0.<ULID>.
func (q *QueryIDKey) MintReadOnlyQueryID(vault string, id [16]byte, idStr string, rc, rs [8]byte) string {
	if q == nil {
		return roPrefixNoKey + idStr
	}
	return roPrefixV1 + idStr + "." + hex.EncodeToString(rc[:]) + "." + hex.EncodeToString(rs[:]) + "." +
		base64.RawURLEncoding.EncodeToString(q.mac(vault, id, rc, rs))
}

// QueryIDKind classifies a query id by SHAPE only. Shape checks leak nothing:
// a well-shaped ro1. id with a wrong MAC is still KindReadOnly here, and its
// MAC failure is a semantic outcome reported only in aggregate.
type QueryIDKind int

const (
	QueryIDInvalid  QueryIDKind = iota
	QueryIDEvent                // a 26-char ULID (the 0x29 event id)
	QueryIDReadOnly             // ro1.…
	QueryIDNoKey                // ro0.<ULID>
)

// ParsedReadOnlyID is the decoded body of an ro1. id. ULID is the 26-char
// string; callers parse it with their ULID type.
type ParsedReadOnlyID struct {
	ULID string
	RC   [8]byte
	RS   [8]byte
	mac  []byte
}

// ClassifyQueryID decides the id's kind by shape. For QueryIDReadOnly the
// parsed body is returned (unverified).
func ClassifyQueryID(s string) (QueryIDKind, ParsedReadOnlyID) {
	switch {
	case len(s) == ulidLen && isCrockford(s):
		return QueryIDEvent, ParsedReadOnlyID{ULID: s}
	case strings.HasPrefix(s, roPrefixNoKey) && len(s) == len(roPrefixNoKey)+ulidLen && isCrockford(s[len(roPrefixNoKey):]):
		return QueryIDNoKey, ParsedReadOnlyID{ULID: s[len(roPrefixNoKey):]}
	case strings.HasPrefix(s, roPrefixV1) && len(s) == ROQueryIDLenV1:
		parts := strings.Split(s[len(roPrefixV1):], ".")
		if len(parts) != 4 || len(parts[0]) != ulidLen || !isCrockford(parts[0]) ||
			len(parts[1]) != roHashHexLen || len(parts[2]) != roHashHexLen || len(parts[3]) != roMACLen {
			return QueryIDInvalid, ParsedReadOnlyID{}
		}
		var p ParsedReadOnlyID
		p.ULID = parts[0]
		rc, err1 := hex.DecodeString(parts[1])
		rs, err2 := hex.DecodeString(parts[2])
		mac, err3 := base64.RawURLEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil || err3 != nil || len(mac) != 16 {
			return QueryIDInvalid, ParsedReadOnlyID{}
		}
		copy(p.RC[:], rc)
		copy(p.RS[:], rs)
		p.mac = mac
		return QueryIDReadOnly, p
	}
	return QueryIDInvalid, ParsedReadOnlyID{}
}

// VerifyReadOnly checks p's MAC for vault and the raw ULID bytes id (the
// parsed form of p.ULID). False with no key.
func (q *QueryIDKey) VerifyReadOnly(vault string, id [16]byte, p ParsedReadOnlyID) bool {
	if q == nil || len(p.mac) != 16 {
		return false
	}
	return hmac.Equal(q.mac(vault, id, p.RC, p.RS), p.mac)
}

// isCrockford reports whether s uses only the Crockford base32 alphabet the
// ULID library accepts (case-insensitive). Overflow is left to the parser.
func isCrockford(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
		case c >= 'a' && c <= 'z' && c != 'i' && c != 'l' && c != 'o' && c != 'u':
		default:
			return false
		}
	}
	return true
}

// Caller identity: the raw credential token and MCP session id of the
// request, carried on ctx from the transport to the engine so the engine (the
// only holder of k_q) can hash them. Never logged, never persisted raw.
type callerIdentityKey struct{}

type callerIdentity struct {
	token   string
	session string
}

// WithCallerIdentity returns ctx carrying the request's credential token and
// session id.
func WithCallerIdentity(ctx context.Context, token, session string) context.Context {
	return context.WithValue(ctx, callerIdentityKey{}, callerIdentity{token: token, session: session})
}

// CallerIdentityFromContext returns the request's token and session id, and
// whether a transport set them at all (only MCP does today — D4).
func CallerIdentityFromContext(ctx context.Context) (token, session string, ok bool) {
	ci, ok := ctx.Value(callerIdentityKey{}).(callerIdentity)
	return ci.token, ci.session, ok
}
