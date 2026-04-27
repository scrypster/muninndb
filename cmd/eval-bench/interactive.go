package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// --- Result types ---

type ScenarioResult struct {
	Name       string
	FeatureOn  float64
	FeatureOff float64
	Delta      float64
	Details    string
}

type InteractiveResults struct {
	EpisodeRecall       ScenarioResult
	ReplayImprovement   ScenarioResult
	SeparationPrecision ScenarioResult
	LociPurity          ScenarioResult
	CompletionRecall    ScenarioResult
}

// --- Synthetic data ---

type syntheticMemory struct {
	concept, content, domain string
	entities                 []mbp.InlineEntity
}

func ent(n, t string) mbp.InlineEntity { return mbp.InlineEntity{Name: n, Type: t} }
func mem(c, ct, d string, e ...mbp.InlineEntity) syntheticMemory {
	return syntheticMemory{c, ct, d, e}
}

func syntheticData() []syntheticMemory {
	return []syntheticMemory{
		mem("Python web frameworks", "Django and Flask are the most popular Python web frameworks. Django provides ORM, admin, auth; Flask offers minimal scaffolding.", "python", ent("Python", "language"), ent("Django", "framework"), ent("Flask", "framework")),
		mem("Python type hints", "PEP 484 introduced type hints. Mypy performs static type checking using annotations.", "python", ent("Python", "language"), ent("type hints", "concept"), ent("mypy", "tool")),
		mem("Python async", "Python asyncio provides an event loop for concurrent I/O with async/await syntax.", "python", ent("Python", "language"), ent("asyncio", "library"), ent("async", "concept")),
		mem("Python testing", "Pytest supports fixtures, parametrize, and assertion introspection for Python testing.", "python", ent("Python", "language"), ent("pytest", "framework"), ent("testing", "concept")),
		mem("Python packaging", "Packages distributed via PyPI using setuptools or poetry with pyproject.toml.", "python", ent("Python", "language"), ent("PyPI", "platform"), ent("packaging", "concept")),
		mem("Python virtualenvs", "Virtualenv and venv isolate dependencies. Each has its own site-packages.", "python", ent("Python", "language"), ent("virtualenv", "tool"), ent("venv", "concept")),
		mem("React patterns", "React uses JSX and hooks like useState/useEffect replacing class lifecycle.", "javascript", ent("JavaScript", "language"), ent("React", "framework"), ent("hooks", "concept")),
		mem("Node.js runtime", "Node.js runs JS on server via V8. Non-blocking I/O handles concurrent connections.", "javascript", ent("JavaScript", "language"), ent("Node.js", "runtime"), ent("V8", "engine")),
		mem("TypeScript types", "TypeScript adds static types: interfaces, generics, union types catch compile errors.", "javascript", ent("JavaScript", "language"), ent("TypeScript", "language"), ent("type system", "concept")),
		mem("JS bundling", "Webpack and esbuild bundle JS modules. Tree-shaking eliminates dead code.", "javascript", ent("JavaScript", "language"), ent("webpack", "tool"), ent("esbuild", "tool")),
		mem("JS testing", "Jest and Vitest provide unit testing with mocking, snapshots, and coverage.", "javascript", ent("JavaScript", "language"), ent("Jest", "framework"), ent("testing", "concept")),
		mem("JS package managers", "npm and pnpm manage dependencies. Lockfiles pin exact versions.", "javascript", ent("JavaScript", "language"), ent("npm", "tool"), ent("pnpm", "tool")),
		mem("PG indexing", "B-tree default in PostgreSQL. GIN/GiST support full-text and JSONB.", "postgresql", ent("PostgreSQL", "database"), ent("indexing", "concept"), ent("B-tree", "data structure")),
		mem("PG query optimization", "EXPLAIN ANALYZE shows execution plans. Proper indexing is key.", "postgresql", ent("PostgreSQL", "database"), ent("query optimization", "concept"), ent("EXPLAIN", "tool")),
		mem("PG replication", "Streaming replication sends WAL to standbys. Synchronous guarantees zero loss.", "postgresql", ent("PostgreSQL", "database"), ent("replication", "concept"), ent("WAL", "concept")),
		mem("PG partitioning", "Table partitioning splits large tables by range, list, or hash.", "postgresql", ent("PostgreSQL", "database"), ent("partitioning", "concept")),
		mem("PG vacuuming", "VACUUM reclaims dead tuples from MVCC. Autovacuum needs tuning.", "postgresql", ent("PostgreSQL", "database"), ent("VACUUM", "operation"), ent("MVCC", "concept")),
		mem("PG connection pooling", "PgBouncer pools connections to prevent exhausting max_connections.", "postgresql", ent("PostgreSQL", "database"), ent("PgBouncer", "tool"), ent("connection pooling", "concept")),
		mem("Redis caching", "Redis stores KV pairs in memory for sub-ms reads. Cache-aside is common.", "redis", ent("Redis", "database"), ent("caching", "concept")),
		mem("Redis pub/sub", "Redis Pub/Sub provides lightweight publish-subscribe messaging.", "redis", ent("Redis", "database"), ent("pub/sub", "concept"), ent("messaging", "concept")),
		mem("Redis streams", "Redis Streams are append-only logs with consumer groups for reliable processing.", "redis", ent("Redis", "database"), ent("streams", "concept"), ent("consumer groups", "concept")),
		mem("Redis clustering", "Redis Cluster distributes data via hash slots with automatic failover.", "redis", ent("Redis", "database"), ent("clustering", "concept"), ent("hash slots", "concept")),
		mem("Redis persistence", "RDB snapshots and AOF provide persistence. Everysec fsync balances durability.", "redis", ent("Redis", "database"), ent("persistence", "concept"), ent("RDB", "concept"), ent("AOF", "concept")),
		mem("Redis data structures", "Strings, lists, sets, sorted sets, hashes, HyperLogLog. Sorted sets for leaderboards.", "redis", ent("Redis", "database"), ent("data structures", "concept"), ent("sorted sets", "concept")),
		mem("Docker basics", "Containers package apps with deps. Dockerfile defines build steps.", "docker", ent("Docker", "platform"), ent("containers", "concept"), ent("Dockerfile", "concept")),
		mem("Docker networking", "Bridge, host, overlay drivers. Bridge isolates on single host.", "docker", ent("Docker", "platform"), ent("networking", "concept"), ent("bridge", "concept")),
		mem("Docker volumes", "Volumes persist data beyond container lifecycle. Bind mounts map host dirs.", "docker", ent("Docker", "platform"), ent("volumes", "concept"), ent("persistence", "concept")),
		mem("Docker Compose", "Compose defines multi-container apps in YAML with services and networks.", "docker", ent("Docker", "platform"), ent("Compose", "tool"), ent("YAML", "format")),
		mem("Docker security", "Non-root, read-only fs, image scanning. Seccomp adds syscall filtering.", "docker", ent("Docker", "platform"), ent("security", "concept"), ent("Seccomp", "tool")),
		mem("Docker multi-stage", "Multi-stage builds separate build/runtime for minimal production images.", "docker", ent("Docker", "platform"), ent("multi-stage builds", "concept")),
	}
}

// episodeTestData returns sessions designed for high within-episode word overlap
// (so hash embedder cosine sim exceeds the threshold) and low between-episode overlap.
func episodeTestData() [][]syntheticMemory {
	return [][]syntheticMemory{
		{ // Session 1: sprint planning meeting
			mem("sprint planning kickoff", "The sprint planning meeting began with reviewing the sprint backlog and velocity.", "meeting"),
			mem("sprint planning stories", "During sprint planning we estimated story points for each sprint task.", "meeting"),
			mem("sprint planning capacity", "Sprint planning capacity based on team availability for the sprint period.", "meeting"),
			mem("sprint planning commitment", "Team committed to 34 story points for this sprint planning cycle.", "meeting"),
		},
		{ // Session 2: database migration
			mem("database migration plan", "The database migration to PostgreSQL requires schema conversion and data validation.", "infra"),
			mem("database migration testing", "Database migration testing verifies data integrity after the migration completes.", "infra"),
			mem("database migration rollback", "A database migration rollback plan ensures we can revert the migration safely.", "infra"),
			mem("database migration timeline", "The database migration timeline spans two weeks with staged migration phases.", "infra"),
		},
		{ // Session 3: API redesign
			mem("API redesign goals", "The API redesign aims to improve API latency and API versioning strategy.", "backend"),
			mem("API redesign endpoints", "During the API redesign we consolidated redundant API endpoints.", "backend"),
			mem("API redesign authentication", "The API redesign introduces token-based API authentication with refresh tokens.", "backend"),
			mem("API redesign documentation", "API redesign documentation covers all new API endpoints and API error codes.", "backend"),
		},
		{ // Session 4: monitoring setup
			mem("monitoring alerting rules", "Setting up monitoring with alerting rules for CPU and memory monitoring thresholds.", "ops"),
			mem("monitoring dashboard design", "The monitoring dashboard visualizes key monitoring metrics in real time.", "ops"),
			mem("monitoring log aggregation", "Monitoring log aggregation collects logs for centralized monitoring analysis.", "ops"),
			mem("monitoring incident response", "Monitoring incident response integrates monitoring alerts with the on-call system.", "ops"),
		},
		{ // Session 5: onboarding process
			mem("onboarding checklist", "The onboarding checklist covers account setup and onboarding orientation sessions.", "hr"),
			mem("onboarding documentation", "Onboarding documentation includes team guides and onboarding FAQ resources.", "hr"),
			mem("onboarding buddy system", "The onboarding buddy system pairs new hires with experienced onboarding mentors.", "hr"),
			mem("onboarding feedback", "Onboarding feedback surveys measure the quality of the onboarding experience.", "hr"),
		},
	}
}

type domainQuery struct{ query, domain string }

func domainQueries() []domainQuery {
	return []domainQuery{
		{"Python web framework development and testing", "python"},
		{"JavaScript React components and Node.js server", "javascript"},
		{"PostgreSQL performance tuning and indexing", "postgresql"},
		{"Redis caching and data structures", "redis"},
		{"Docker container security and networking", "docker"},
	}
}

// --- Helpers ---

// ftsSettleTime is enough for the async FTS worker to flush its batch (100ms interval).
const ftsSettleTime = 250 * time.Millisecond

// writeAll writes memories at the given indices to the engine, returning IDs.
func writeAll(ctx context.Context, eng *evalEngine, data []syntheticMemory, indices []int) ([]string, error) {
	ids := make([]string, 0, len(indices))
	for _, idx := range indices {
		m := data[idx]
		resp, err := eng.WriteWithEmbedding(ctx, &mbp.WriteRequest{
			Concept: m.concept, Content: m.content, Vault: "eval",
			Entities: m.entities, Tags: []string{m.domain},
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, resp.ID)
	}
	return ids, nil
}

// episodeRecall calls CompleteEpisode from a seed and returns recall vs expected.
func episodeRecall(ctx context.Context, eng *evalEngine, seed string, expected map[string]bool) float64 {
	members, err := eng.Engine.CompleteEpisode(ctx, "eval", seed)
	if err != nil {
		return 0
	}
	returned := make(map[string]bool, len(members))
	for _, m := range members {
		returned[m.ID] = true
	}
	return setOverlap(returned, expected)
}

// --- Main entry point ---

func RunInteractive(ctx context.Context, embedder activation.Embedder, verbose bool) (*InteractiveResults, error) {
	results := &InteractiveResults{}
	baseDir := os.TempDir()
	if verbose {
		fmt.Fprintf(os.Stderr, "[interactive] starting 5 hippocampal scenarios\n")
	}

	type scenarioFunc func(context.Context, string, activation.Embedder, bool) (ScenarioResult, error)
	scenarios := []struct {
		name string
		fn   scenarioFunc
		dest *ScenarioResult
	}{
		{"episodes", scenarioEpisodes, &results.EpisodeRecall},
		{"replay", scenarioReplay, &results.ReplayImprovement},
		{"separation", scenarioSeparation, &results.SeparationPrecision},
		{"loci", scenarioLoci, &results.LociPurity},
		{"completion", scenarioCompletion, &results.CompletionRecall},
	}
	for i, s := range scenarios {
		r, err := s.fn(ctx, baseDir, embedder, verbose)
		if err != nil {
			return nil, fmt.Errorf("scenario %d (%s): %w", i+1, s.name, err)
		}
		*s.dest = r
	}
	return results, nil
}

// --- Scenario 1: Episode Segmentation ---

// writeEpisodeSession writes a slice of syntheticMemory directly, returning IDs.
func writeEpisodeSession(ctx context.Context, eng *evalEngine, mems []syntheticMemory) ([]string, error) {
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		resp, err := eng.WriteWithEmbedding(ctx, &mbp.WriteRequest{
			Concept: m.concept, Content: m.content, Vault: "eval",
			Tags: []string{m.domain},
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, resp.ID)
	}
	return ids, nil
}

func scenarioEpisodes(ctx context.Context, baseDir string, embedder activation.Embedder, verbose bool) (ScenarioResult, error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 1] episode segmentation\n")
	}
	epCfg := cognitive.DefaultEpisodeConfig()
	epCfg.SimilarityThreshold = 0.10 // hash embedder needs lower threshold
	onCfg := cognitive.HippocampalConfig{EnableEpisodes: true, EpisodeConfig: epCfg}
	onEng, err := NewEvalEngine(baseDir, &onCfg, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer onEng.Close()
	offEng, err := NewEvalEngine(baseDir, nil, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer offEng.Close()

	episodes := episodeTestData()[:3] // first 3 episodes
	onIDs := make([][]string, len(episodes))
	offIDs := make([][]string, len(episodes))

	for si, ep := range episodes {
		if onIDs[si], err = writeEpisodeSession(ctx, onEng, ep); err != nil {
			return ScenarioResult{}, err
		}
		if offIDs[si], err = writeEpisodeSession(ctx, offEng, ep); err != nil {
			return ScenarioResult{}, err
		}
		time.Sleep(300 * time.Millisecond) // boundary between sessions
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 1] waiting 7s for episode worker...\n")
	}
	time.Sleep(7 * time.Second)

	var onSum, offSum float64
	for si := range episodes {
		onSum += episodeRecall(ctx, onEng, onIDs[si][1], toSet(onIDs[si]))
		offSum += episodeRecall(ctx, offEng, offIDs[si][1], toSet(offIDs[si]))
	}
	on, off := onSum/float64(len(episodes)), offSum/float64(len(episodes))
	return ScenarioResult{
		Name: "Episode Segmentation", FeatureOn: on, FeatureOff: off, Delta: on - off,
		Details: fmt.Sprintf("%d sessions x4; ON=%.3f OFF=%.3f", len(episodes), on, off),
	}, nil
}

// --- Scenario 2: Replay Consolidation ---

func scenarioReplay(ctx context.Context, baseDir string, embedder activation.Embedder, verbose bool) (ScenarioResult, error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 2] replay consolidation\n")
	}
	onCfg := cognitive.HippocampalConfig{
		EnableReplay: true,
		ReplayConfig: cognitive.ReplayConfig{Interval: 100 * time.Millisecond, MaxEngrams: 50},
	}
	eng, err := NewEvalEngine(baseDir, &onCfg, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer eng.Close()

	data := syntheticData()[:10]
	allIdx := make([]int, 10)
	for i := range allIdx {
		allIdx[i] = i
	}
	allIDs, err := writeAll(ctx, eng, data, allIdx)
	if err != nil {
		return ScenarioResult{}, err
	}
	time.Sleep(ftsSettleTime) // let FTS worker flush

	// Activate targets 5x to build co-activation.
	query := "Python web frameworks type hints async programming"
	for i := 0; i < 5; i++ {
		eng.Engine.Activate(ctx, &mbp.ActivateRequest{Context: []string{query}, Vault: "eval", MaxResults: 10, Threshold: 0.01}) //nolint:errcheck
	}

	targets := toSet(allIDs[:3])
	beforeResp, _ := eng.Engine.Activate(ctx, &mbp.ActivateRequest{Context: []string{query}, Vault: "eval", MaxResults: 10, Threshold: 0.01})
	beforeMRR := meanReciprocalRank(beforeResp.Activations, targets)

	// Run replay worker for one cycle.
	replayStore := cognitive.NewReplayStoreAdapter(eng.Store)
	rw := cognitive.NewReplayWorker(cognitive.ReplayConfig{Interval: 100 * time.Millisecond, MaxEngrams: 50}, eng.Engine, replayStore)
	rCtx, rCancel := context.WithCancel(ctx)
	go rw.Run(rCtx)
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 2] waiting for replay cycle...\n")
	}
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for done := false; !done; {
		select {
		case <-tick.C:
			done = rw.Metrics().CyclesCompleted >= 1
		case <-deadline:
			done = true
		case <-ctx.Done():
			done = true
		}
	}
	rw.Stop()
	rCancel()

	afterResp, _ := eng.Engine.Activate(ctx, &mbp.ActivateRequest{Context: []string{query}, Vault: "eval", MaxResults: 10, Threshold: 0.01})
	afterMRR := meanReciprocalRank(afterResp.Activations, targets)

	return ScenarioResult{
		Name: "Replay Consolidation", FeatureOn: afterMRR, FeatureOff: beforeMRR, Delta: afterMRR - beforeMRR,
		Details: fmt.Sprintf("3 targets 5x; before=%.3f after=%.3f cycles=%d", beforeMRR, afterMRR, rw.Metrics().CyclesCompleted),
	}, nil
}

// --- Scenario 3: Pattern Separation ---

func scenarioSeparation(ctx context.Context, baseDir string, embedder activation.Embedder, verbose bool) (ScenarioResult, error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 3] pattern separation\n")
	}
	onCfg := cognitive.HippocampalConfig{EnableSeparation: true, SeparationConfig: cognitive.DefaultSeparationConfig()}
	onEng, err := NewEvalEngine(baseDir, &onCfg, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer onEng.Close()
	offEng, err := NewEvalEngine(baseDir, nil, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer offEng.Close()

	data := syntheticData()
	allIdx := make([]int, len(data))
	for i := range allIdx {
		allIdx[i] = i
	}

	onIDs, err := writeAll(ctx, onEng, data, allIdx)
	if err != nil {
		return ScenarioResult{}, err
	}
	offIDs, err := writeAll(ctx, offEng, data, allIdx)
	if err != nil {
		return ScenarioResult{}, err
	}
	time.Sleep(ftsSettleTime)

	// Build ID-to-domain maps.
	onDom, offDom := make(map[string]string), make(map[string]string)
	for i, m := range data {
		onDom[onIDs[i]] = m.domain
		offDom[offIDs[i]] = m.domain
	}

	// Use MaxResults=10 so the top-5 seeds define entity context and the
	// remaining 5 are candidates for separation penalty. With MaxResults=5,
	// all results are seeds (separationSeedTopN=5) and nothing gets penalized.
	var onSum, offSum float64
	for _, q := range domainQueries() {
		onResp, _ := onEng.Engine.Activate(ctx, &mbp.ActivateRequest{Context: []string{q.query}, Vault: "eval", MaxResults: 10, Threshold: 0.01})
		onSum += domainPrecisionAtK(onResp.Activations, q.domain, onDom, 10)
		offResp, _ := offEng.Engine.Activate(ctx, &mbp.ActivateRequest{Context: []string{q.query}, Vault: "eval", MaxResults: 10, Threshold: 0.01})
		offSum += domainPrecisionAtK(offResp.Activations, q.domain, offDom, 10)
	}
	n := float64(len(domainQueries()))
	on, off := onSum/n, offSum/n
	return ScenarioResult{
		Name: "Pattern Separation", FeatureOn: on, FeatureOff: off, Delta: on - off,
		Details: fmt.Sprintf("30 mem, 5 domains; ON prec@10=%.3f OFF=%.3f", on, off),
	}, nil
}

// --- Scenario 4: Emergent Loci ---

func scenarioLoci(ctx context.Context, baseDir string, embedder activation.Embedder, verbose bool) (ScenarioResult, error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 4] emergent loci\n")
	}
	cfg := cognitive.HippocampalConfig{EnableLoci: true, LociConfig: cognitive.DefaultLociConfig()}
	eng, err := NewEvalEngine(baseDir, &cfg, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer eng.Close()

	data := syntheticData()
	allIdx := make([]int, len(data))
	for i := range allIdx {
		allIdx[i] = i
	}
	if _, err = writeAll(ctx, eng, data, allIdx); err != nil {
		return ScenarioResult{}, err
	}

	loci, err := eng.Engine.DetectLoci(ctx, "eval", 1)
	if err != nil {
		return ScenarioResult{}, err
	}

	entityDom := buildEntityDomainMap(data)
	var totalPurity float64
	count := 0
	for _, locus := range loci {
		if locus.Size < 2 {
			continue
		}
		votes := make(map[string]int)
		for _, member := range locus.Members {
			if d, ok := entityDom[member]; ok {
				votes[d]++
			}
		}
		if len(votes) == 0 {
			continue
		}
		maxV, total := 0, 0
		for _, v := range votes {
			total += v
			if v > maxV {
				maxV = v
			}
		}
		totalPurity += float64(maxV) / float64(total)
		count++
	}
	purity := 0.0
	if count > 0 {
		purity = totalPurity / float64(count)
	}
	return ScenarioResult{
		Name: "Emergent Loci", FeatureOn: purity, FeatureOff: 0, Delta: purity,
		Details: fmt.Sprintf("%d communities, avg purity=%.3f", count, purity),
	}, nil
}

// --- Scenario 5: Pattern Completion ---

func scenarioCompletion(ctx context.Context, baseDir string, embedder activation.Embedder, verbose bool) (ScenarioResult, error) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 5] pattern completion\n")
	}
	epCfg := cognitive.DefaultEpisodeConfig()
	epCfg.SimilarityThreshold = 0.10 // hash embedder needs lower threshold
	onCfg := cognitive.HippocampalConfig{
		EnableEpisodes: true, EpisodeConfig: epCfg,
		EnableCompletion: true,
	}
	onEng, err := NewEvalEngine(baseDir, &onCfg, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer onEng.Close()
	offEng, err := NewEvalEngine(baseDir, nil, embedder)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer offEng.Close()

	episodes := episodeTestData()
	onEpIDs := make([][]string, len(episodes))
	offEpIDs := make([][]string, len(episodes))

	for ei, ep := range episodes {
		if onEpIDs[ei], err = writeEpisodeSession(ctx, onEng, ep); err != nil {
			return ScenarioResult{}, err
		}
		if offEpIDs[ei], err = writeEpisodeSession(ctx, offEng, ep); err != nil {
			return ScenarioResult{}, err
		}
		time.Sleep(300 * time.Millisecond) // boundary between episodes
	}

	if verbose {
		fmt.Fprintf(os.Stderr, "[scenario 5] waiting 7s for episode worker...\n")
	}
	time.Sleep(7 * time.Second)

	var onRecall, offRecall, onOrder, offOrder float64
	for ei := range episodes {
		onRecall += episodeRecall(ctx, onEng, onEpIDs[ei][1], toSet(onEpIDs[ei]))
		offRecall += episodeRecall(ctx, offEng, offEpIDs[ei][1], toSet(offEpIDs[ei]))

		// Order correctness from CompleteEpisode.
		onMembers, _ := onEng.Engine.CompleteEpisode(ctx, "eval", onEpIDs[ei][1])
		onOrd := make([]string, 0, len(onMembers))
		for _, m := range onMembers {
			onOrd = append(onOrd, m.ID)
		}
		onOrder += orderCorrectness(onOrd, onEpIDs[ei])

		offMembers, _ := offEng.Engine.CompleteEpisode(ctx, "eval", offEpIDs[ei][1])
		offOrd := make([]string, 0, len(offMembers))
		for _, m := range offMembers {
			offOrd = append(offOrd, m.ID)
		}
		offOrder += orderCorrectness(offOrd, offEpIDs[ei])
	}
	n := float64(len(episodes))
	on, off := onRecall/n, offRecall/n
	return ScenarioResult{
		Name: "Pattern Completion", FeatureOn: on, FeatureOff: off, Delta: on - off,
		Details: fmt.Sprintf("5 eps x4; ON recall=%.3f order=%.3f, OFF recall=%.3f order=%.3f", on, onOrder/n, off, offOrder/n),
	}, nil
}

// --- Reporting ---

func printInteractiveReport(results *InteractiveResults) {
	all := []ScenarioResult{
		results.EpisodeRecall, results.ReplayImprovement,
		results.SeparationPrecision, results.LociPurity, results.CompletionRecall,
	}
	fmt.Fprintf(os.Stderr, "\nInteractive Hippocampal Benchmark\n")
	fmt.Fprintf(os.Stderr, "══════════════════════════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "%-24s  %7s  %7s  %7s  %s\n", "Scenario", "ON", "OFF", "Delta", "Details")
	fmt.Fprintf(os.Stderr, "──────────────────────────────────────────────────────────────\n")
	for _, s := range all {
		sign := "+"
		if s.Delta < 0 {
			sign = ""
		}
		fmt.Fprintf(os.Stderr, "%-24s  %7.3f  %7.3f  %s%5.3f  %s\n",
			s.Name, s.FeatureOn, s.FeatureOff, sign, s.Delta, s.Details)
	}
	fmt.Fprintf(os.Stderr, "══════════════════════════════════════════════════════════════\n")
	var sum float64
	for _, s := range all {
		sum += s.Delta
	}
	fmt.Fprintf(os.Stderr, "Average delta: %.3f\n\n", sum/float64(len(all)))
}

// --- Scoring utilities ---

func toSet(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

func setOverlap(returned, expected map[string]bool) float64 {
	if len(expected) == 0 {
		return 0
	}
	hit := 0
	for id := range expected {
		if returned[id] {
			hit++
		}
	}
	return float64(hit) / float64(len(expected))
}

func meanReciprocalRank(items []mbp.ActivationItem, targets map[string]bool) float64 {
	if len(targets) == 0 {
		return 0
	}
	var sum float64
	for id := range targets {
		for rank, item := range items {
			if item.ID == id {
				sum += 1.0 / float64(rank+1)
				break
			}
		}
	}
	return sum / float64(len(targets))
}

func domainPrecision(items []mbp.ActivationItem, domain string, idDom map[string]string) float64 {
	return domainPrecisionAtK(items, domain, idDom, len(items))
}

func domainPrecisionAtK(items []mbp.ActivationItem, domain string, idDom map[string]string, k int) float64 {
	if len(items) == 0 || k <= 0 {
		return 0
	}
	n := k
	if n > len(items) {
		n = len(items)
	}
	correct := 0
	for _, item := range items[:n] {
		if idDom[item.ID] == domain {
			correct++
		}
	}
	return float64(correct) / float64(n)
}

func orderCorrectness(returned, expected []string) float64 {
	if len(returned) < 2 {
		return 0
	}
	pos := make(map[string]int, len(expected))
	for i, id := range expected {
		pos[id] = i
	}
	concordant, total := 0, 0
	for i := 0; i < len(returned)-1; i++ {
		pi, okI := pos[returned[i]]
		pj, okJ := pos[returned[i+1]]
		if okI && okJ {
			total++
			if pi < pj {
				concordant++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(concordant) / float64(total)
}

func buildEntityDomainMap(data []syntheticMemory) map[string]string {
	counts := make(map[string]map[string]int)
	for _, m := range data {
		for _, e := range m.entities {
			if counts[e.Name] == nil {
				counts[e.Name] = make(map[string]int)
			}
			counts[e.Name][m.domain]++
		}
	}
	result := make(map[string]string, len(counts))
	for entity, dc := range counts {
		domains := make([]string, 0, len(dc))
		for d := range dc {
			domains = append(domains, d)
		}
		sort.Strings(domains)
		best, bestC := "", 0
		for _, d := range domains {
			if dc[d] > bestC {
				bestC = dc[d]
				best = d
			}
		}
		result[entity] = best
	}
	return result
}
