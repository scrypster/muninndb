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
// 0x29 event. A read-only recall writes nothing, so its id must verify itself
// — including WHICH memories it returned, and in which part of the response:
//
//	ro2.<ULID 26>.<rc 16 hex>.<rs 16 hex>.<tags base64url>.<atags base64url>.<mac 22 base64url>
//	tag_i = HMAC-SHA256(k_q, "row" 0x00 vault 0x00 ULID(16) engramID_i(16))[:4]
//	tags  = tag_1 ‖ … ‖ tag_k        (matched rows, served order, 4 bytes each)
//	atags = the same tag for each item of the COG-35 `associated` block
//	mac   = HMAC-SHA256(k_q, "ro2" 0x00 vault 0x00 ULID(16) rc(8) rs(8) u16be(len(tags)) tags atags)[:16]
//	k_q   = HMAC-SHA256(auth_secret, "muninn/query-id/v1")
//
// ro1 (the layout before the associated block had its own section) is still
// ACCEPTED: one tag list, the same tag and k_q, mac over "ro1" … tags with no
// length. An ro1 id minted by a build with the associated block carries the
// block's ids after the matched rows, indistinguishably — the decoder reports
// it as Legacy so the label pipeline records what it cannot know as unknown
// (feedback_labels.go), never as a matched-row rank. Nothing mints ro1 now.
//
// The row tags make a read-only label's membership AND rank checkable
// statelessly: a label is accepted only for a memory whose tag is in the id,
// and its rank is the tag's position. Without them a caller could mint
// unlimited read-only ids (they are cheap and write nothing) and use each to
// label ANY memory (security review of slice 2a; this is the spec's deferred
// D3, pulled forward). A 4-byte keyed tag cannot be searched offline (k_q is
// secret); an online forgery matches one of k rows with probability k/2^32
// per minted id, and its only effect is a label row — access bumps are
// capped per (credential, memory, UTC day) on every path. Exact verification
// would need the full 16-byte ids (22 chars per row); 4 bytes keeps the id at
// ~142 chars for 10 rows and ~355 for 50.
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
	roPrefixV2      = "ro2."
	roPrefixNoKey   = "ro0."
	ulidLen         = 26
	roHashHexLen    = 16
	roMACLen        = 22 // base64url, no padding, of 16 bytes
	// ROTagLen is the per-row tag length in bytes.
	ROTagLen = 4
	// ROMaxRows is the most rows a read-only id carries tags for. MCP caps a
	// recall at 100, so every MCP row is tagged; on transports that allow a
	// larger recall, rows 101+ are UNTAGGED and a label for one is rejected
	// with an explicit reason (untagged_row), never treated as an ordinary
	// miss.
	ROMaxRows = 100
	roMaxRows = ROMaxRows
	// roMaxAssocRows bounds the associated section (the block carries at most
	// 3; the bound is the parser's, with headroom).
	roMaxAssocRows = 8
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

func (q *QueryIDKey) mac(vault string, id [16]byte, rc, rs [8]byte, tags []byte) []byte {
	m := hmac.New(sha256.New, q.k)
	m.Write([]byte("ro1"))
	m.Write([]byte{0})
	m.Write([]byte(vault))
	m.Write([]byte{0})
	m.Write(id[:])
	m.Write(rc[:])
	m.Write(rs[:])
	m.Write(tags)
	return m.Sum(nil)[:16]
}

func (q *QueryIDKey) macV2(vault string, id [16]byte, rc, rs [8]byte, tags, atags []byte) []byte {
	m := hmac.New(sha256.New, q.k)
	m.Write([]byte("ro2"))
	m.Write([]byte{0})
	m.Write([]byte(vault))
	m.Write([]byte{0})
	m.Write(id[:])
	m.Write(rc[:])
	m.Write(rs[:])
	m.Write([]byte{byte(len(tags) >> 8), byte(len(tags))})
	m.Write(tags)
	m.Write(atags)
	return m.Sum(nil)[:16]
}

func (q *QueryIDKey) rowTag(vault string, id, engramID [16]byte) []byte {
	m := hmac.New(sha256.New, q.k)
	m.Write([]byte("row"))
	m.Write([]byte{0})
	m.Write([]byte(vault))
	m.Write([]byte{0})
	m.Write(id[:])
	m.Write(engramID[:])
	return m.Sum(nil)[:ROTagLen]
}

// MintReadOnlyQueryID mints a read-only (ro2) query id for the matched rows
// the recall returned, in served order, and the ids of its associated block.
// idStr is the 26-char ULID string of id. With no key it returns ro0.<ULID>.
func (q *QueryIDKey) MintReadOnlyQueryID(vault string, id [16]byte, idStr string, rc, rs [8]byte, rows, assoc [][16]byte) string {
	if q == nil {
		return roPrefixNoKey + idStr
	}
	tags, atags := q.tagList(vault, id, rows, roMaxRows), q.tagList(vault, id, assoc, roMaxAssocRows)
	return roPrefixV2 + idStr + "." + hex.EncodeToString(rc[:]) + "." + hex.EncodeToString(rs[:]) + "." +
		base64.RawURLEncoding.EncodeToString(tags) + "." + base64.RawURLEncoding.EncodeToString(atags) + "." +
		base64.RawURLEncoding.EncodeToString(q.macV2(vault, id, rc, rs, tags, atags))
}

// MintReadOnlyQueryIDV1 mints the LEGACY ro1 layout (one tag list). No
// production path mints it; it exists so the decoder's handling of ro1 ids
// already issued stays pinned by tests.
func (q *QueryIDKey) MintReadOnlyQueryIDV1(vault string, id [16]byte, idStr string, rc, rs [8]byte, rows [][16]byte) string {
	if q == nil {
		return roPrefixNoKey + idStr
	}
	tags := q.tagList(vault, id, rows, roMaxRows)
	return roPrefixV1 + idStr + "." + hex.EncodeToString(rc[:]) + "." + hex.EncodeToString(rs[:]) + "." +
		base64.RawURLEncoding.EncodeToString(tags) + "." +
		base64.RawURLEncoding.EncodeToString(q.mac(vault, id, rc, rs, tags))
}

func (q *QueryIDKey) tagList(vault string, id [16]byte, rows [][16]byte, max int) []byte {
	if len(rows) > max {
		rows = rows[:max]
	}
	tags := make([]byte, 0, len(rows)*ROTagLen)
	for _, r := range rows {
		tags = append(tags, q.rowTag(vault, id, r)...)
	}
	return tags
}

// QueryIDKind classifies a query id by SHAPE only. Shape checks leak nothing:
// a well-shaped ro1. id with a wrong MAC is still KindReadOnly here, and its
// MAC failure is a semantic outcome reported only in aggregate.
type QueryIDKind int

const (
	QueryIDInvalid  QueryIDKind = iota
	QueryIDEvent                // a 26-char ULID (the 0x29 event id)
	QueryIDReadOnly             // ro2.… or the legacy ro1.…
	QueryIDNoKey                // ro0.<ULID>
)

// ParsedReadOnlyID is the decoded body of an ro2. (or legacy ro1.) id. ULID
// is the 26-char string; callers parse it with their ULID type.
type ParsedReadOnlyID struct {
	ULID  string
	RC    [8]byte
	RS    [8]byte
	v1    bool
	tags  []byte
	atags []byte
	mac   []byte
}

// Rows is the number of row tags in the matched section: for ro2 the matched
// rows the recall served in `memories` (the result set size); for a Legacy
// id every tag, which may include up to 3 associated items after the
// matched rows.
func (p ParsedReadOnlyID) Rows() int { return len(p.tags) / ROTagLen }

// Legacy reports an ro1 id: its single tag list cannot say where the matched
// rows end and associated items begin.
func (p ParsedReadOnlyID) Legacy() bool { return p.v1 }

// ClassifyQueryID decides the id's kind by shape. For QueryIDReadOnly the
// parsed body is returned (unverified).
func ClassifyQueryID(s string) (QueryIDKind, ParsedReadOnlyID) {
	switch {
	case len(s) == ulidLen && isCrockford(s):
		return QueryIDEvent, ParsedReadOnlyID{ULID: s}
	case strings.HasPrefix(s, roPrefixNoKey) && len(s) == len(roPrefixNoKey)+ulidLen && isCrockford(s[len(roPrefixNoKey):]):
		return QueryIDNoKey, ParsedReadOnlyID{ULID: s[len(roPrefixNoKey):]}
	case strings.HasPrefix(s, roPrefixV1) && len(s) <= len(roPrefixV1)+ulidLen+2*(1+roHashHexLen)+1+base64.RawURLEncoding.EncodedLen(roMaxRows*ROTagLen)+1+roMACLen:
		return parseReadOnly(strings.Split(s[len(roPrefixV1):], "."), true)
	case strings.HasPrefix(s, roPrefixV2) && len(s) <= len(roPrefixV2)+ulidLen+2*(1+roHashHexLen)+1+base64.RawURLEncoding.EncodedLen(roMaxRows*ROTagLen)+
		1+base64.RawURLEncoding.EncodedLen(roMaxAssocRows*ROTagLen)+1+roMACLen:
		return parseReadOnly(strings.Split(s[len(roPrefixV2):], "."), false)
	}
	return QueryIDInvalid, ParsedReadOnlyID{}
}

// parseReadOnly decodes the dot-separated body of an ro1 (5 parts) or ro2
// (6 parts: an associated section before the MAC) id.
func parseReadOnly(parts []string, v1 bool) (QueryIDKind, ParsedReadOnlyID) {
	want := 6
	if v1 {
		want = 5
	}
	if len(parts) != want || len(parts[0]) != ulidLen || !isCrockford(parts[0]) ||
		len(parts[1]) != roHashHexLen || len(parts[2]) != roHashHexLen || len(parts[want-1]) != roMACLen {
		return QueryIDInvalid, ParsedReadOnlyID{}
	}
	p := ParsedReadOnlyID{ULID: parts[0], v1: v1}
	rc, err1 := hex.DecodeString(parts[1])
	rs, err2 := hex.DecodeString(parts[2])
	tags, err3 := base64.RawURLEncoding.DecodeString(parts[3])
	mac, err4 := base64.RawURLEncoding.DecodeString(parts[want-1])
	var atags []byte
	var err5 error
	if !v1 {
		atags, err5 = base64.RawURLEncoding.DecodeString(parts[4])
	}
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || len(mac) != 16 ||
		len(tags)%ROTagLen != 0 || len(atags)%ROTagLen != 0 || len(atags) > roMaxAssocRows*ROTagLen {
		return QueryIDInvalid, ParsedReadOnlyID{}
	}
	copy(p.RC[:], rc)
	copy(p.RS[:], rs)
	p.tags, p.atags, p.mac = tags, atags, mac
	return QueryIDReadOnly, p
}

// VerifyReadOnly checks p's MAC for vault and the raw ULID bytes id (the
// parsed form of p.ULID). False with no key.
func (q *QueryIDKey) VerifyReadOnly(vault string, id [16]byte, p ParsedReadOnlyID) bool {
	if q == nil || len(p.mac) != 16 {
		return false
	}
	if p.v1 {
		return hmac.Equal(q.mac(vault, id, p.RC, p.RS, p.tags), p.mac)
	}
	return hmac.Equal(q.macV2(vault, id, p.RC, p.RS, p.tags, p.atags), p.mac)
}

// ReadOnlyRank returns the 1-based position of engramID in a VERIFIED
// read-only id's matched section (ro2: its served rank in `memories`; Legacy:
// its position in the one tag list), or ok=false when that section carries
// no tag for it.
func (q *QueryIDKey) ReadOnlyRank(vault string, id [16]byte, p ParsedReadOnlyID, engramID [16]byte) (rank int, ok bool) {
	if q == nil {
		return 0, false
	}
	return tagPos(p.tags, q.rowTag(vault, id, engramID))
}

// ReadOnlyAssociated returns the 1-based position of engramID in a VERIFIED
// ro2 id's associated section, or ok=false (always for a Legacy id).
func (q *QueryIDKey) ReadOnlyAssociated(vault string, id [16]byte, p ParsedReadOnlyID, engramID [16]byte) (pos int, ok bool) {
	if q == nil {
		return 0, false
	}
	return tagPos(p.atags, q.rowTag(vault, id, engramID))
}

func tagPos(tags, want []byte) (int, bool) {
	for i := 0; i+ROTagLen <= len(tags); i += ROTagLen {
		if hmac.Equal(tags[i:i+ROTagLen], want) {
			return i/ROTagLen + 1, true
		}
	}
	return 0, false
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
