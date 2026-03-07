// eval-semantic: seeds 100 work/professional engrams into a live MuninnDB instance
// and runs 3 semantic retrieval probes where the query shares zero keywords with
// the expected target engrams. Demonstrates that MuninnDB finds memories through
// meaning, not keyword overlap.
//
// Usage:
//
//	go run ./cmd/eval-semantic -url http://127.0.0.1:8750 -token mdb_yourtoken
//	go run ./cmd/eval-semantic -url http://127.0.0.1:8750 -token mdb_yourtoken -vault my-eval
//
// The vault is cleared between runs by default (-clear=true).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─── REST API types ─────────────────────────────────────────────────────────

type writeReq struct {
	Concept    string   `json:"concept"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags,omitempty"`
	Confidence float32  `json:"confidence"`
	Vault      string   `json:"vault"`
}

// weights mirrors mbp.Weights — field names must match Go struct names exactly
// because the server's json.Unmarshal uses field names (no json tags on that struct).
type weights struct {
	SemanticSimilarity float32 `json:"SemanticSimilarity"`
	FullTextRelevance  float32 `json:"FullTextRelevance"`
	DecayFactor        float32 `json:"DecayFactor"`
	HebbianBoost       float32 `json:"HebbianBoost"`
	AccessFrequency    float32 `json:"AccessFrequency"`
	Recency            float32 `json:"Recency"`
}

type activateReq struct {
	Context    []string `json:"context"`
	MaxResults int      `json:"max_results"`
	Vault      string   `json:"vault"`
	Weights    *weights `json:"Weights,omitempty"`
}

type activationItem struct {
	ID      string  `json:"ID"`
	Concept string  `json:"Concept"`
	Content string  `json:"Content"`
	Score   float32 `json:"Score"`
}

type activateResp struct {
	Activations []activationItem `json:"Activations"`
}

// ─── Probe definition ────────────────────────────────────────────────────────

// probe defines a zero-keyword-overlap semantic retrieval test.
type probe struct {
	Query     string
	Targets   []string // concept names expected in top 3
	Rationale string   // why there is no keyword overlap
}

// ─── The 3 target engrams ────────────────────────────────────────────────────
//
// Each is designed so that its words share zero meaningful overlap with its
// corresponding probe query. The semantic match comes from meaning alone.

var targetEngrams = []writeReq{
	// Matched by probe 1: "unable to stop thinking about work when I should be resting"
	// Zero overlap: unable/stop/thinking/work/resting vs opened/laptop/quick/look/hours/dinner/cold/family/alerts/settled/refreshing/brain/walk/green/night
	{
		Concept:    "Late evening status check spiral",
		Content:    "Opened the laptop at 10pm just to take a quick look. Two hours later I'm still there. Dinner cold. Family in bed. The alerts had settled but I kept refreshing. Something in my brain wouldn't let me walk away until everything was green. This happens every night.",
		Tags:       []string{"burnout", "evening"},
		Confidence: 0.9,
	},

	// Matched by probe 2: "depleted after putting in all the effort solo with zero acknowledgment"
	// Zero overlap: depleted/putting/effort/solo/zero/acknowledgment vs crossed/marathon/finish/line/myself/legs/gave/mile/twenty-four/nobody/waiting/drove/home/told/medal/drawer
	{
		Concept:    "Crossed the marathon finish line by myself",
		Content:    "Finished the run by myself. My legs gave out around mile twenty-four. Nobody was waiting at the finish line. Drove home and didn't tell anyone. Just put the medal in a drawer.",
		Tags:       []string{"personal", "endurance", "solitude"},
		Confidence: 0.9,
	},

	// Matched by probe 3: "completely overwhelmed by how beautiful it was"
	// Zero overlap: completely/overwhelmed/beautiful vs came/over/ridge/stopped/whole/valley/below/color/spread/across/everything/expected/stood/until/cold
	{
		Concept:    "Top of the ridge, valley below",
		Content:    "Came over the ridge and stopped. The whole valley was below, color spread across everything. I hadn't expected it. Stood there until I got cold.",
		Tags:       []string{"personal", "outdoors", "awe"},
		Confidence: 0.9,
	},
}

// ─── The 3 probes ────────────────────────────────────────────────────────────

var probes = []probe{
	{
		Query:   "can't disconnect from the job when it's time to relax",
		Targets: []string{"Late evening status check spiral"},
		Rationale: `query="can't/disconnect/job/time/relax"  target="opened/laptop/quick/look/hours/dinner/cold/family/alerts/settled/refreshing/brain/walk/away/green/night"`,
	},
	{
		Query:   "depleted from shouldering it all with zero acknowledgment",
		Targets: []string{"Crossed the marathon finish line by myself"},
		Rationale: `query="depleted/shouldering/all/zero/acknowledgment"  target="finished/run/myself/legs/gave/mile/nobody/waiting/drove/home/medal/drawer"`,
	},
	{
		Query:   "completely overwhelmed by how beautiful it was",
		Targets: []string{"Top of the ridge, valley below"},
		Rationale: `query="completely/overwhelmed/beautiful"  target="came/over/ridge/stopped/whole/valley/below/color/spread/everything/expected/stood/cold"`,
	},
}

// ─── 27 filler engrams ───────────────────────────────────────────────────────
//
// Three clusters of 9: work (before T1), running/physical (before T2),
// nature/awe (before T3). Total corpus: 27 + 3 targets = 30 items.
//
// With N=30, HNSW candidate count = clamp(sqrt(30), 30, 200) = 30 = N,
// so all items are candidates — ensuring fair semantic retrieval.

func fillerEngrams() []writeReq {
	return []writeReq{
		// 1-9: work — meeting notes and project updates (noise cluster near target 1)
		{Concept: "Tuesday standup", Content: "Standup was quick. Auth work, Q4 roadmap discussion, nothing blocking anyone.", Tags: []string{"work", "meetings"}, Confidence: 0.8},
		{Concept: "Weekly product sync", Content: "Weekly sync with product. Feature flag timeline got pushed two sprints. Roadmap updated.", Tags: []string{"work", "meetings"}, Confidence: 0.8},
		{Concept: "Sprint planning day", Content: "Sprint planning: 14 story points accepted. Two tickets carried from last sprint. Scope looks realistic.", Tags: []string{"work", "meetings", "process"}, Confidence: 0.8},
		{Concept: "Friday retro", Content: "Retro was useful. Two action items carried from last week finally closed. Team wants shorter standups.", Tags: []string{"work", "meetings", "process"}, Confidence: 0.8},
		{Concept: "Q4 all-hands", Content: "All-hands covered org chart changes and the new engineering director. No impact to our team.", Tags: []string{"work", "meetings"}, Confidence: 0.8},
		{Concept: "Architecture review: event bus", Content: "Architecture review for the new event bus proposal. Three competing designs evaluated. Decision deferred to next week.", Tags: []string{"work", "meetings", "engineering"}, Confidence: 0.8},
		{Concept: "Monthly all-hands", Content: "Monthly all-hands: revenue numbers solid, headcount freeze announced for Q1. Engineering unaffected for now.", Tags: []string{"work", "meetings"}, Confidence: 0.8},
		{Concept: "Roadmap prioritization", Content: "Product roadmap prioritization session. Three features cut from Q4. Engineering capacity was the constraint.", Tags: []string{"work", "meetings", "product"}, Confidence: 0.8},
		{Concept: "Incident postmortem: cache invalidation", Content: "Postmortem for the cache invalidation incident. Root cause: stale TTL config after the CDN migration. Three action items assigned.", Tags: []string{"work", "meetings", "engineering"}, Confidence: 0.8},

		// (position 10 is target 1 — inserted in buildCorpus)

		// 10-18: personal — solo physical effort (cluster near target 2)
		{Concept: "Half marathon, no one at the finish", Content: "Did the half marathon solo. Nobody from the training group came. Crossed the line by myself and took the train home.", Tags: []string{"personal", "running"}, Confidence: 0.8},
		{Concept: "Morning runs, invisible effort", Content: "Ran every morning this week. Nobody asks about it. The training is invisible until race day.", Tags: []string{"personal", "running"}, Confidence: 0.8},
		{Concept: "Ten miles after work, legs gone", Content: "Ten miles after work. Sore by mile six but kept going. Nobody waiting at the end.", Tags: []string{"personal", "running"}, Confidence: 0.8},
		{Concept: "Cycling century, completely spent", Content: "Finished the hundred-mile ride solo. My legs were gone by the last twenty miles. Nobody at home knew how hard it was.", Tags: []string{"personal", "cycling"}, Confidence: 0.8},
		{Concept: "New personal record, nobody asked", Content: "Set a new 5K PR today. Nobody asked about it. Just logged it and moved on.", Tags: []string{"personal", "running"}, Confidence: 0.8},
		{Concept: "Gym at 5am, no one else there", Content: "In the gym at 5am before anyone else. Did the full session. Nobody tracks it but me.", Tags: []string{"personal", "fitness"}, Confidence: 0.8},
		{Concept: "Solo hike, ate lunch at the summit", Content: "Hiked the ridge solo this weekend. Nobody came with. Good views. Ate lunch by myself at the top.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Run in the rain, nobody there after", Content: "Ran in the rain this morning. Passed three people the whole route. Nobody there when I got back.", Tags: []string{"personal", "running"}, Confidence: 0.8},
		{Concept: "Finished the race, drained, by myself", Content: "Crossed the line completely spent. My whole body was shot. Nobody I knew was there. Stood by the barriers for a few minutes, then left.", Tags: []string{"personal", "running", "endurance"}, Confidence: 0.8},

		// (position 20 is target 2 — inserted in buildCorpus)

		// 19-27: personal — nature and awe (cluster near target 3)
		{Concept: "Wildflower meadow, unexpected colors", Content: "Took the long route through the meadow. Colors I hadn't expected. Stopped to look at least a dozen times. Glad I went that way.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Fog on the water at dawn", Content: "Woke early and walked to the shore. Fog sitting low on everything. Silent and white and strange. Left feeling something I couldn't name.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Sunset from the hilltop", Content: "Made it to the top just as the sun was going. The whole sky changed color. Other people were there but nobody was talking.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Canyon rim at dusk", Content: "Stood at the rim as the light was going. The colors deepened into everything below. Didn't want to turn around.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Lightning over the water", Content: "Lightning came from three directions at once over the water. Stood on the porch in the rain just watching. Couldn't go inside.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Swallows at dusk over the field", Content: "Watched the swallows over the field for an hour. Moving in patterns that seemed to mean something. Couldn't look away.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Old tree in fall light", Content: "Big old tree at the edge of the park, all color. Sat under it for longer than I intended. The light through the leaves.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Milky Way visible for the first time", Content: "First time seeing the Milky Way clearly. Drove out past the lights, lay on the hood of the car. That felt like it mattered.", Tags: []string{"personal", "outdoors"}, Confidence: 0.8},
		{Concept: "Morning light through the curtains", Content: "Woke before the alarm and just lay there watching the light change on the ceiling. Gold, then white. Didn't get up for a while.", Tags: []string{"personal", "home"}, Confidence: 0.8},
	}
}

// buildCorpus inserts targets at positions 10, 20, 28 among 27 filler items.
// Total: 30 items — ensures HNSW scans all items (clamp(sqrt(30), 30, 200) = 30).
func buildCorpus() []writeReq {
	filler := fillerEngrams()
	corpus := make([]writeReq, 0, 30)
	for i, e := range filler {
		switch i {
		case 9: // inject target 1 after first 9 work items
			corpus = append(corpus, targetEngrams[0])
		case 18: // inject target 2 after 9 running items
			corpus = append(corpus, targetEngrams[1])
		case 26: // inject target 3 after 8 nature items (last filler)
			corpus = append(corpus, targetEngrams[2])
		}
		corpus = append(corpus, e)
	}
	return corpus
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

var (
	serverURL string
	authToken string
	client    = &http.Client{Timeout: 30 * time.Second}
)

func doJSON(method, path string, body any, out any) error {
	var r *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		r, err = http.NewRequest(method, serverURL+path, bytes.NewReader(b))
	} else {
		r, err = http.NewRequest(method, serverURL+path, nil)
	}
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		r.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("HTTP %d: %v", resp.StatusCode, errBody)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func seed(vault string, engram writeReq) error {
	engram.Vault = vault
	return doJSON("POST", "/api/engrams", engram, nil)
}

func activate(vault, query string, maxResults int, w *weights) ([]activationItem, error) {
	req := activateReq{
		Context:    []string{query},
		MaxResults: maxResults,
		Vault:      vault,
		Weights:    w,
	}
	var resp activateResp
	if err := doJSON("POST", "/api/activate", req, &resp); err != nil {
		return nil, err
	}
	return resp.Activations, nil
}

var (
	wFTS = &weights{FullTextRelevance: 1.0, SemanticSimilarity: 0}
	wSem = &weights{SemanticSimilarity: 1.0, FullTextRelevance: 0}
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func isTarget(concept string, targets []string) bool {
	for _, t := range targets {
		if strings.EqualFold(concept, t) {
			return true
		}
	}
	return false
}

func targetRank(items []activationItem, targets []string) int {
	for i, item := range items {
		if isTarget(item.Concept, targets) {
			return i + 1
		}
	}
	return -1
}

func snippet(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	url := flag.String("url", "http://127.0.0.1:8475", "MuninnDB server URL")
	tok := flag.String("token", "", "API token (Bearer)")
	vlt := flag.String("vault", "eval-demo", "Vault name")
	skipSeed := flag.Bool("skip-seed", false, "Skip seeding (reuse existing vault)")
	compare := flag.Bool("compare", false, "Run FTS-only vs semantic side-by-side comparison")
	flag.Parse()

	serverURL = strings.TrimRight(*url, "/")
	authToken = *tok
	vault := *vlt

	fmt.Printf("MuninnDB — Semantic Retrieval Eval\n")
	fmt.Printf("════════════════════════════════════════════════════════\n")
	fmt.Printf("Server: %s\n", serverURL)
	fmt.Printf("Vault:  %s\n", vault)
	fmt.Println()

	// ── Phase 1: seed ────────────────────────────────────────────────────────

	if !*skipSeed {
		corpus := buildCorpus()
		fmt.Printf("Seeding %d engrams...\n", len(corpus))
		for i, e := range corpus {
			if err := seed(vault, e); err != nil {
				fmt.Fprintf(os.Stderr, "  [error] seed %d %q: %v\n", i+1, e.Concept, err)
				os.Exit(1)
			}
			if (i+1)%10 == 0 {
				fmt.Printf("  %d/%d\n", i+1, len(corpus))
			}
		}
		fmt.Printf("Seeded %d engrams.\n\n", len(corpus))

		fmt.Println("Waiting 2s for FTS indexing to settle...")
		time.Sleep(2 * time.Second)
		fmt.Println()
	} else {
		fmt.Println("Skipping seed (using existing vault).")
	}

	// ── Phase 2: probes ───────────────────────────────────────────────────────

	if *compare {
		runCompare(vault)
	} else {
		runProbes(vault, nil)
	}
}

// runProbes runs all probes with the given weights (nil = server default).
func runProbes(vault string, w *weights) int {
	fmt.Printf("── Phase 2: Semantic Probes ─────────────────────────────\n\n")
	fmt.Printf("Each query shares ZERO keywords with its target engram.\n")
	fmt.Printf("Retrieval succeeds only through semantic meaning.\n\n")

	passed := 0
	for i, p := range probes {
		fmt.Printf("Probe %d of %d\n", i+1, len(probes))
		fmt.Printf("  Query:     %q\n", p.Query)
		fmt.Printf("  Target:    %q\n", strings.Join(p.Targets, ", "))
		fmt.Printf("  No-overlap proof: %s\n\n", p.Rationale)

		results, err := activate(vault, p.Query, 10, w)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [error] activate: %v\n", err)
			continue
		}

		rank := targetRank(results, p.Targets)
		top := 5
		if top > len(results) {
			top = len(results)
		}

		fmt.Printf("  Top %d results:\n", top)
		for j := 0; j < top; j++ {
			item := results[j]
			marker := "  "
			if isTarget(item.Concept, p.Targets) {
				marker = "★ "
			}
			fmt.Printf("    %s%d. [%.3f] %-40s  %s\n",
				marker, j+1, item.Score, item.Concept, snippet(item.Content, 60))
		}
		fmt.Println()

		if rank >= 1 && rank <= 3 {
			fmt.Printf("  ✓ PASS  — target found at rank %d\n", rank)
			passed++
		} else if rank > 3 {
			fmt.Printf("  ✗ FAIL  — target found but only at rank %d (want ≤3)\n", rank)
		} else {
			fmt.Printf("  ✗ FAIL  — target not found in top 10\n")
		}
		fmt.Println()
	}

	fmt.Printf("── Summary ──────────────────────────────────────────────\n\n")
	fmt.Printf("  Probes: %d  |  Passed: %d  |  Failed: %d\n\n", len(probes), passed, len(probes)-passed)

	if passed == len(probes) {
		fmt.Println("  ALL PROBES PASSED")
		fmt.Println("  MuninnDB retrieved semantically related memories with zero keyword overlap.")
	} else {
		fmt.Printf("  %d/%d probes passed.\n", passed, len(probes))
		fmt.Println("  Check embedding provider — semantic mode requires a real embedding model.")
		fmt.Println("  FTS-only mode will not pass zero-overlap probes by design.")
	}
	fmt.Println()
	return passed
}

// runCompare runs each probe twice — FTS-only vs semantic — and prints a stacked comparison.
func runCompare(vault string) {
	fmt.Printf("── FTS vs Semantic Comparison ───────────────────────────\n\n")
	fmt.Printf("Same query. Same corpus. Two retrieval strategies.\n")
	fmt.Printf("Queries share ZERO keywords with their targets.\n\n")

	type probeResult struct {
		rank   int
		top5   []activationItem
		passed bool
	}

	ftsScores := make([]probeResult, len(probes))
	semScores := make([]probeResult, len(probes))

	for i, p := range probes {
		fmt.Printf("┌─ Probe %d ──────────────────────────────────────────────\n", i+1)
		fmt.Printf("│  Query:  %q\n", p.Query)
		fmt.Printf("│  Target: %q\n", strings.Join(p.Targets, ", "))
		fmt.Printf("│  No-overlap: %s\n", p.Rationale)
		fmt.Printf("│\n")

		ftsRes, err := activate(vault, p.Query, 10, wFTS)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [error] FTS activate: %v\n", err)
			continue
		}
		semRes, err := activate(vault, p.Query, 10, wSem)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [error] semantic activate: %v\n", err)
			continue
		}

		ftsRank := targetRank(ftsRes, p.Targets)
		semRank := targetRank(semRes, p.Targets)

		ftsScores[i] = probeResult{rank: ftsRank, top5: top(ftsRes, 5), passed: ftsRank >= 1 && ftsRank <= 3}
		semScores[i] = probeResult{rank: semRank, top5: top(semRes, 5), passed: semRank >= 1 && semRank <= 3}

		// FTS block
		fmt.Printf("│  FTS-ONLY (keyword matching)\n")
		fmt.Printf("│  ──────────────────────────────────────────────────────\n")
		printResultBlock(ftsScores[i].top5, p.Targets)
		fmt.Printf("│  → %s\n", ftsVerdict(ftsRank))
		fmt.Printf("│\n")

		// Semantic block
		fmt.Printf("│  SEMANTIC (meaning-based, bge-small-en-v1.5 embeddings)\n")
		fmt.Printf("│  ──────────────────────────────────────────────────────\n")
		printResultBlock(semScores[i].top5, p.Targets)
		fmt.Printf("│  → %s\n", semVerdict(semRank))
		fmt.Printf("└────────────────────────────────────────────────────────\n\n")
	}

	// ── Scorecard ─────────────────────────────────────────────────────────────

	fmt.Printf("── Scorecard ────────────────────────────────────────────\n\n")
	fmt.Printf("  %-44s  %-10s  %-10s\n", "Probe query", "FTS", "Semantic")
	fmt.Printf("  %-44s  %-10s  %-10s\n", strings.Repeat("─", 44), strings.Repeat("─", 10), strings.Repeat("─", 10))

	ftsPassed, semPassed := 0, 0
	for i, p := range probes {
		ftsR := rankStr(ftsScores[i].rank)
		semR := rankStr(semScores[i].rank)
		if ftsScores[i].passed {
			ftsPassed++
		}
		if semScores[i].passed {
			semPassed++
		}
		fmt.Printf("  %-44s  %-10s  %-10s\n", snippet(p.Query, 44), ftsR, semR)
	}
	fmt.Println()
	fmt.Printf("  FTS passed:      %d / %d\n", ftsPassed, len(probes))
	fmt.Printf("  Semantic passed: %d / %d\n\n", semPassed, len(probes))

	switch {
	case semPassed == len(probes):
		fmt.Println("  ✓ ALL SEMANTIC PROBES PASSED")
		fmt.Println("  Keywords didn't match — meaning did.")
	case semPassed > ftsPassed:
		fmt.Printf("  Semantic outperformed FTS by %d probe(s).\n", semPassed-ftsPassed)
		fmt.Println("  Keywords didn't match — meaning did.")
	case semPassed == ftsPassed && semPassed > 0:
		fmt.Println("  Both modes matched equally. Results may overlap at this corpus size.")
	case ftsPassed > semPassed:
		fmt.Println("  ⚠  No embedding model detected — semantic mode falling back to hash vectors.")
		fmt.Println("  Start muninn with an embedding key to see the real difference:")
		fmt.Println("    MUNINN_VOYAGE_KEY=...  muninn start")
		fmt.Println("    MUNINN_OPENAI_KEY=...  muninn start")
	default:
		fmt.Println("  No probes passed in either mode.")
		fmt.Println("  Check server connection and embedding provider.")
	}
	fmt.Println()
}

func printResultBlock(items []activationItem, targets []string) {
	n := len(items)
	if n == 0 {
		fmt.Printf("│    (no results)\n")
		return
	}
	for j, item := range items {
		mark := "  "
		if isTarget(item.Concept, targets) {
			mark = "★ "
		}
		fmt.Printf("│  %s%d. [%.3f] %-32s  %s\n",
			mark, j+1, item.Score, snippet(item.Concept, 32), snippet(item.Content, 45))
	}
}

func top(items []activationItem, n int) []activationItem {
	if len(items) < n {
		return items
	}
	return items[:n]
}

func rankStr(rank int) string {
	if rank == -1 {
		return "not found"
	}
	verdict := ""
	if rank <= 3 {
		verdict = " ✓"
	}
	return fmt.Sprintf("#%d%s", rank, verdict)
}

func ftsVerdict(rank int) string {
	if rank == -1 {
		return "✗ FAIL — target not found (no keyword match)"
	}
	if rank <= 3 {
		return fmt.Sprintf("✓ PASS — rank %d", rank)
	}
	return fmt.Sprintf("✗ FAIL — rank %d (keyword match too weak)", rank)
}

func semVerdict(rank int) string {
	if rank == -1 {
		return "✗ FAIL — target not found (no embedding model?)"
	}
	if rank <= 3 {
		return fmt.Sprintf("✓ PASS — rank %d (semantic match, zero keyword overlap)", rank)
	}
	return fmt.Sprintf("✗ FAIL — rank %d", rank)
}
