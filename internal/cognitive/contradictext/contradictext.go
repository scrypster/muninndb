// Package contradictext is a model-free, pure-function text contradiction
// detector. Given two short statements it decides whether they carry an
// ACTUAL conflict signal — not whether they are similar. This distinction is
// the whole point: two statements can be embedding-near-identical paraphrases
// and NOT conflict ("returns 200 on success" vs "responds OK when it
// succeeds"), while two dissimilar-looking statements DO ("auth tokens never
// expire" vs "auth tokens expire after 24 hours"). Similarity is never
// consulted here.
//
// It is deliberately separate from internal/cognitive/contradict.go, which
// detects contradictions between association RELATION TYPES over the
// association matrix (Supports vs Contradicts, etc.). That mechanism operates
// on graph edges; this one operates on raw text and has no storage or engine
// dependency. Both the recall self-knowledge surface and the Push consume this
// text detector.
//
// Two detectors are ported (matching the proven Python miner: precision 1.00,
// recall 0.93, paraphrase false-positive rate 0.000):
//
//  1. Polarity / negation flip — same-subject statements where one negates the
//     other (a negation cue in scope over a shared predicate) or asserts a
//     known antonym of the other ("enabled" vs "disabled").
//  2. Numeric / value mismatch — same-subject statements that assert a
//     genuinely SWAPPED value in a comparable slot (100 vs 250; true vs false).
//     A swap requires a value present in A but not B AND a value present in B
//     but not A, so "returns 200 on success" vs "responds OK ..." (only one
//     side has a value) is never flagged.
//
// Both detectors are gated on a shared-subject test so unrelated statements are
// never compared. Everything is deterministic, allocation-light, and cheap
// enough to run across a full recall result set (~190 pairs) in well under a
// millisecond.
package contradictext

import (
	"regexp"
	"sort"
	"strings"
)

// Kind classifies which detector fired. Empty when there is no conflict.
const (
	KindNone     = ""
	KindPolarity = "polarity"
	KindNumeric  = "numeric"
)

// Detector holds the (English, hand-built) lexicons and the enable flags for
// each detector. The zero value is not useful; construct one with New (or use
// the package-level Conflict, which shares a single default Detector). Callers
// may build a Detector with New and extend its exported maps before first use;
// the maps are read-only during detection so a configured Detector is safe to
// share across goroutines.
type Detector struct {
	// Stopwords are dropped when computing the shared-subject overlap.
	Stopwords map[string]struct{}
	// Negations are cues that flip polarity ("not", "never", "no", ...).
	Negations map[string]struct{}
	// Antonyms maps a word to its polarity opposite, both directions.
	Antonyms map[string]string
	// Booleans maps recognised boolean literals to a canonical value so
	// "true"/"false" and "yes"/"no" participate in the numeric swap test.
	Booleans map[string]string

	// EnableNumeric / EnablePolarity toggle the individual detectors. Both
	// default true; they exist so tests can prove each detector is load-bearing
	// (disable it and the case it uniquely catches must stop being flagged).
	EnableNumeric  bool
	EnablePolarity bool
}

// numberRe matches integers, decimals and percentages, with an optional sign.
// Compiled once; FindAllString is the only per-call allocation of note.
var numberRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?%?`)

// tokenRe splits on any run of non-alphanumeric characters (so "req/s" ->
// "req","s" and "24h" stays "24h"); it is used for word-level tokenisation.
var wordSplit = func(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
}

// New returns a Detector with the default lexicons and both detectors enabled.
func New() *Detector {
	d := &Detector{
		Stopwords:      make(map[string]struct{}),
		Negations:      make(map[string]struct{}),
		Antonyms:       make(map[string]string),
		Booleans:       make(map[string]string),
		EnableNumeric:  true,
		EnablePolarity: true,
	}
	for _, w := range defaultStopwords {
		d.Stopwords[w] = struct{}{}
	}
	for _, w := range defaultNegations {
		d.Negations[w] = struct{}{}
	}
	for _, p := range defaultAntonymPairs {
		d.Antonyms[p[0]] = p[1]
		d.Antonyms[p[1]] = p[0]
	}
	for k, v := range defaultBooleans {
		d.Booleans[k] = v
	}
	return d
}

// std is the shared default Detector backing the package-level Conflict.
var std = New()

// Conflict reports whether a and b carry an actual contradiction signal, using
// the package default Detector. See Detector.Conflict.
func Conflict(a, b string) (isConflict bool, reason, kind string) {
	return std.Conflict(a, b)
}

// Conflict reports whether a and b carry an actual contradiction signal.
// It returns (false, "", KindNone) when there is no conflict — including for
// paraphrases, which are similar but not conflicting. When a conflict is found
// kind is KindNumeric or KindPolarity and reason is a short human explanation.
func (d *Detector) Conflict(a, b string) (isConflict bool, reason, kind string) {
	ta := d.tokenize(a)
	tb := d.tokenize(b)

	// Shared-subject gate: the two statements must be about the same thing
	// before any conflict signal is meaningful. This is what keeps unrelated
	// pairs (even ones that both happen to contain numbers) from being flagged.
	if !d.sameSubject(ta, tb) {
		return false, "", KindNone
	}

	// Numeric / value swap first (concrete), then polarity.
	if d.EnableNumeric {
		if ok, why := d.numericConflict(ta, tb); ok {
			return true, why, KindNumeric
		}
	}
	if d.EnablePolarity {
		if ok, why := d.polarityConflict(ta, tb); ok {
			return true, why, KindPolarity
		}
	}
	return false, "", KindNone
}

// tokens is the parsed view of one statement.
type tokens struct {
	words   []string            // all lowercased alphanumeric words, in order
	set     map[string]struct{} // set of words
	subject map[string]struct{} // words minus stopwords/negations/numbers/booleans
	values  map[string]struct{} // numeric literals + canonical boolean values
	negated bool                // statement contains a negation cue
}

func (d *Detector) tokenize(s string) tokens {
	s = strings.ToLower(s)
	words := strings.FieldsFunc(s, wordSplit)

	t := tokens{
		words:   words,
		set:     make(map[string]struct{}, len(words)),
		subject: make(map[string]struct{}, len(words)),
		values:  make(map[string]struct{}),
	}
	for _, w := range words {
		t.set[w] = struct{}{}
		if _, isNeg := d.Negations[w]; isNeg {
			t.negated = true
		}
		if bv, isBool := d.Booleans[w]; isBool {
			t.values[bv] = struct{}{}
		}
	}
	// Numeric literals come straight off the raw (lowercased) string so signs,
	// decimals and percentages survive the alphanumeric split.
	for _, n := range numberRe.FindAllString(s, -1) {
		t.values[n] = struct{}{}
	}
	// Subject words: drop stopwords, negation cues, pure numbers and booleans so
	// the differing signal words don't distort the overlap.
	for _, w := range words {
		if _, ok := d.Stopwords[w]; ok {
			continue
		}
		if _, ok := d.Negations[w]; ok {
			continue
		}
		if _, ok := d.Booleans[w]; ok {
			continue
		}
		if isNumberWord(w) {
			continue
		}
		t.subject[w] = struct{}{}
	}
	return t
}

// sameSubject requires at least two shared subject words. Two is enough to bind
// the statements to a common referent while still rejecting unrelated pairs.
func (d *Detector) sameSubject(a, b tokens) bool {
	shared := 0
	// Iterate the smaller set.
	small, large := a.subject, b.subject
	if len(large) < len(small) {
		small, large = large, small
	}
	for w := range small {
		if _, ok := large[w]; ok {
			shared++
			if shared >= 2 {
				return true
			}
		}
	}
	return false
}

// numericConflict fires on a genuine value SWAP: some value asserted by a is
// absent from b AND some value asserted by b is absent from a. This models
// "different value in the same slot" without flagging a mere extra number.
func (d *Detector) numericConflict(a, b tokens) (bool, string) {
	if len(a.values) == 0 || len(b.values) == 0 {
		return false, ""
	}
	onlyA := diff(a.values, b.values)
	onlyB := diff(b.values, a.values)
	if len(onlyA) == 0 || len(onlyB) == 0 {
		return false, ""
	}
	return true, "value mismatch: " + strings.Join(onlyA, ",") + " vs " + strings.Join(onlyB, ",")
}

// polarityConflict fires when the two same-subject statements carry opposite
// polarity: either a known antonym pair spans them, or exactly one of them
// negates a shared predicate (negation XOR over a common subject word).
func (d *Detector) polarityConflict(a, b tokens) (bool, string) {
	// Antonym across the pair.
	for w := range a.set {
		if opp, ok := d.Antonyms[w]; ok {
			if _, present := b.set[opp]; present {
				return true, "antonym pair: " + w + "/" + opp
			}
		}
	}
	// Negation XOR over a shared subject predicate. Exactly one side negates,
	// and (via the subject gate already passed) they share a predicate word, so
	// one asserts what the other denies.
	if a.negated != b.negated {
		if pred, ok := firstShared(a.subject, b.subject); ok {
			return true, "negation flip on shared predicate: " + pred
		}
	}
	return false, ""
}

// --- helpers ---------------------------------------------------------------

func isNumberWord(w string) bool {
	// A word is numeric if it is fully consumed by numberRe.
	loc := numberRe.FindString(w)
	return loc == w
}

// diff returns the sorted members of x not present in y.
func diff(x, y map[string]struct{}) []string {
	var out []string
	for v := range x {
		if _, ok := y[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// firstShared returns a deterministic shared member of a and b, if any.
func firstShared(a, b map[string]struct{}) (string, bool) {
	small, large := a, b
	if len(large) < len(small) {
		small, large = large, small
	}
	var shared []string
	for w := range small {
		if _, ok := large[w]; ok {
			shared = append(shared, w)
		}
	}
	if len(shared) == 0 {
		return "", false
	}
	sort.Strings(shared)
	return shared[0], true
}

// --- default lexicons ------------------------------------------------------

var defaultStopwords = []string{
	"a", "an", "the", "is", "are", "was", "were", "be", "been", "being",
	"to", "of", "on", "in", "for", "when", "it", "its", "at", "by", "as",
	"and", "or", "with", "that", "this", "these", "those", "after", "before",
	"will", "has", "have", "had", "does", "do", "did", "than", "then", "so",
	"but", "if", "into", "over", "per", "from", "up",
}

var defaultNegations = []string{
	"not", "no", "never", "none", "neither", "nor", "without", "cannot",
	"cant", "wont", "dont", "doesnt", "isnt", "arent", "wasnt", "werent",
	"nt", "unable", "fails", "fail",
}

// defaultAntonymPairs are hand-built polarity opposites. Kept small and
// English-only; a caller may extend Detector.Antonyms for its domain. "on/off"
// is deliberately omitted because "on" is a stopword.
var defaultAntonymPairs = [][2]string{
	{"enabled", "disabled"},
	{"enable", "disable"},
	{"active", "inactive"},
	{"allowed", "denied"},
	{"allow", "deny"},
	{"valid", "invalid"},
	{"present", "absent"},
	{"granted", "revoked"},
	{"locked", "unlocked"},
	{"online", "offline"},
	{"mutable", "immutable"},
	{"public", "private"},
	{"required", "optional"},
	{"mandatory", "optional"},
	{"permanent", "temporary"},
	{"success", "failure"},
	{"open", "closed"},
}

// defaultBooleans map boolean literals to a canonical value so a true/false
// flip registers as a numeric-style value swap.
// The canonical values contain ':' so they can never collide with a numeric
// literal (numberRe never emits ':') or an ordinary word token.
var defaultBooleans = map[string]string{
	"true":  "bool:true",
	"false": "bool:false",
	"yes":   "bool:true",
	"no":    "bool:false",
}
