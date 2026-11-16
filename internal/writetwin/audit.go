package writetwin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// FlagLogMsg is the msg of the one structured INFO line logged per flagged
// pair. The audit reads it back; renaming it breaks the audit.
const FlagLogMsg = "write twin flagged"

// LoggedPair is one flagged pair read back from the server log.
type LoggedPair struct {
	VaultWS string // 16-hex workspace prefix
	NewID   storage.ULID
	HeadID  storage.ULID
	Rule    string
	Shown   bool
	At      time.Time // when it was logged
}

// LogSpan is the time range a server log covers (first and last timestamped
// line of any kind) — the shadow period the audit's write count spans.
type LogSpan struct {
	First, Last time.Time
	// Starts are the times the check started in a process (StartLogMsg
	// lines). For one Window after each, the ring is warming — writes made
	// before the start are not in it — so the audit treats that stretch as
	// blind and reports it separately.
	Starts []time.Time
}

// StartLogMsg is logged once when a server starts the check; the audit reads
// it back to find each warm-up stretch. Renaming it breaks the audit.
const StartLogMsg = "write-time twin check enabled"

func (s LogSpan) blind(at time.Time) bool {
	for _, st := range s.Starts {
		if !at.Before(st) && at.Before(st.Add(Window)) {
			return true
		}
	}
	return false
}

// ParseFlagLog reads FlagLogMsg lines from a server log (slog text or JSON
// handler output) and returns the pairs plus the span the whole log covers.
// Unparseable lines are skipped.
func ParseFlagLog(r io.Reader) ([]LoggedPair, LogSpan, error) {
	var out []LoggedPair
	var span LogSpan
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, FlagLogMsg) {
			if at, ok := lineTime(line); ok {
				span.extend(at)
				if strings.Contains(line, StartLogMsg) && parseLogLine(line)["msg"] == StartLogMsg {
					span.Starts = append(span.Starts, at)
				}
			}
			continue
		}
		kv := parseLogLine(line)
		if at, err := time.Parse(time.RFC3339Nano, kv["time"]); err == nil {
			span.extend(at)
		}
		if kv["msg"] != FlagLogMsg {
			continue
		}
		nid, err1 := storage.ParseULID(kv["new_id"])
		hid, err2 := storage.ParseULID(kv["head_id"])
		at, err3 := time.Parse(time.RFC3339Nano, kv["time"])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		shown, _ := strconv.ParseBool(kv["shown"])
		out = append(out, LoggedPair{VaultWS: kv["vault_ws"], NewID: nid, HeadID: hid, Rule: kv["rule"], Shown: shown, At: at})
	}
	return out, span, sc.Err()
}

func (s *LogSpan) extend(at time.Time) {
	if s.First.IsZero() || at.Before(s.First) {
		s.First = at
	}
	if at.After(s.Last) {
		s.Last = at
	}
}

// lineTime reads the time= / "time": field of any log line cheaply.
func lineTime(line string) (time.Time, bool) {
	if at, err := time.Parse(time.RFC3339Nano, parseLogLine(line)["time"]); err == nil {
		return at, true
	}
	return time.Time{}, false
}

// parseLogLine accepts a JSON object line or a logfmt line (key=value,
// key="quoted value"), returning string values.
func parseLogLine(line string) map[string]string {
	kv := map[string]string{}
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			for k, v := range m {
				switch t := v.(type) {
				case string:
					kv[k] = t
				case bool:
					kv[k] = strconv.FormatBool(t)
				case float64:
					kv[k] = strconv.FormatFloat(t, 'f', -1, 64)
				}
			}
		}
		return kv
	}
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			j := i + 1
			var b strings.Builder
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' && j+1 < len(line) {
					j++
				}
				b.WriteByte(line[j])
				j++
			}
			kv[key] = b.String()
			i = j + 1
		} else {
			j := strings.IndexByte(line[i:], ' ')
			if j < 0 {
				j = len(line) - i
			}
			kv[key] = line[i : i+j]
			i += j
		}
	}
	return kv
}

// AuditReport is the live-phase store-quality outcome (v3 §6.ii): of the
// recent-twin pairs flagged at least MinAge ago, how many are still
// unresolved in the store, per 100 writes made over the same period.
type AuditReport struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	PairsLogged      int       `json:"pairs_logged"`
	PairsMature      int       `json:"pairs_mature"` // flagged ≥ MinAge ago
	Unresolved       int       `json:"unresolved"`
	Writes           int       `json:"writes"` // non-successor writes in [From, To]
	UnresolvedPer100 float64   `json:"unresolved_per_100_writes"`
	MinAgeDays       int       `json:"min_age_days"`
	// The first hour after each process start is blind (the ring was
	// warming); its pairs and writes are reported here, not in the rate.
	WarmupStretches     int `json:"warmup_stretches"`
	WarmupPairsExcluded int `json:"warmup_pairs_excluded"`
	WarmupWritesExclude int `json:"warmup_writes_excluded"`
}

// ErrNoPairs is returned when the log holds no pairs for the vault.
var ErrNoPairs = errors.New("no flagged pairs for this vault in the log")

// Audit counts unresolved near-twin pairs. A pair (W, E) is UNRESOLVED when,
// read from the store now: both are still live (StateActive), neither has a
// supersedes or contradicts edge to the other, and neither was forgotten.
// Only rule=recent pairs count (the rule the block shows), each pair once,
// and only pairs flagged at least minAge ago. The denominator is every
// non-successor write made over the same period: span (the log's coverage)
// cut at now−minAge, so a write too young to have a mature verdict is not
// counted either.
func Audit(ctx context.Context, store *storage.PebbleStore, ws [8]byte, pairs []LoggedPair, span LogSpan, now time.Time, minAge time.Duration) (*AuditReport, error) {
	wsHex := hexWS(ws)
	rep := &AuditReport{MinAgeDays: int(minAge / (24 * time.Hour))}
	cut := now.Add(-minAge)
	rep.From, rep.To = span.First, span.Last
	rep.WarmupStretches = len(span.Starts)
	if rep.To.IsZero() || rep.To.After(cut) {
		rep.To = cut
	}
	seen := map[[2]storage.ULID]bool{}
	var mature []LoggedPair
	for _, p := range pairs {
		if p.VaultWS != wsHex || p.Rule != "recent" {
			continue
		}
		k := [2]storage.ULID{p.NewID, p.HeadID}
		if seen[k] {
			continue
		}
		seen[k] = true
		rep.PairsLogged++
		if p.At.After(cut) {
			continue
		}
		if span.blind(p.At) {
			rep.WarmupPairsExcluded++
			continue
		}
		mature = append(mature, p)
		if rep.From.IsZero() || p.At.Before(rep.From) {
			rep.From = p.At
		}
	}
	if rep.PairsLogged == 0 {
		return rep, ErrNoPairs
	}
	rep.PairsMature = len(mature)
	for _, p := range mature {
		unresolved, err := pairUnresolved(ctx, store, ws, p.NewID, p.HeadID)
		if err != nil {
			return nil, err
		}
		if unresolved {
			rep.Unresolved++
		}
	}
	if len(mature) > 0 || rep.WarmupPairsExcluded > 0 {
		all, err := loadReplayEngrams(ctx, store, ws)
		if err != nil {
			return nil, err
		}
		for i := range all {
			c := all[i].created
			if !c.Before(rep.From) && !c.After(rep.To) && !all[i].successor {
				if span.blind(c) {
					rep.WarmupWritesExclude++
					continue
				}
				rep.Writes++
			}
		}
		if rep.Writes > 0 {
			rep.UnresolvedPer100 = 100 * float64(rep.Unresolved) / float64(rep.Writes)
		}
	}
	return rep, nil
}

func pairUnresolved(ctx context.Context, store *storage.PebbleStore, ws [8]byte, a, b storage.ULID) (bool, error) {
	ctx = storage.ContextWithNoAccessCacheStamp(ctx)
	for _, id := range []storage.ULID{a, b} {
		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return false, nil // hard-deleted: resolved
			}
			return false, err
		}
		if eng.State != storage.StateActive {
			return false, nil
		}
	}
	fwd, err := store.GetAssociations(ctx, ws, []storage.ULID{a, b}, 0)
	if err != nil {
		return false, err
	}
	for src, other := range map[storage.ULID]storage.ULID{a: b, b: a} {
		for _, as := range fwd[src] {
			if as.TargetID == other && (as.RelType == storage.RelSupersedes || as.RelType == storage.RelContradicts) {
				return false, nil
			}
		}
	}
	return true, nil
}

func hexWS(ws [8]byte) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 16)
	for i, c := range ws {
		b[2*i] = digits[c>>4]
		b[2*i+1] = digits[c&0xf]
	}
	return string(b)
}

// HexWS renders a workspace prefix the way the flag log line does.
func HexWS(ws [8]byte) string { return hexWS(ws) }
