package mcp

import (
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/auth"
)

type engineStats struct {
	EngramCount int64
	VaultCount  int
}

// generateGuide renders the per-vault guide. debtSection is the pre-rendered
// COG-29 debt readout (see contradictionDebtGuideSection) — empty when the
// vault carries no unresolved declared contradiction, which is the overwhelming
// default and adds zero bytes.
func generateGuide(vaultName string, resolved auth.ResolvedPlasticity, stats engineStats, debtSection string) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "# MuninnDB Memory Guide for vault: %s\n\n", vaultName)

	// Memory Strategy
	b.WriteString("## Memory Strategy\n\n")
	switch resolved.BehaviorMode {
	case "prompted":
		b.WriteString("Only store memories when the user explicitly asks you to remember something. ")
		b.WriteString("Use recall when the user asks you to search their memory.\n")
	case "selective":
		b.WriteString("Automatically remember decisions, errors, and their resolutions. ")
		b.WriteString("For other information, only remember when the user asks. ")
		b.WriteString("Always recall before starting tasks that relate to previous work.\n")
	case "custom":
		if resolved.BehaviorInstructions != "" {
			b.WriteString(resolved.BehaviorInstructions)
			b.WriteString("\n")
		} else {
			b.WriteString("Custom behavior mode is configured but no instructions were provided. ")
			b.WriteString("Falling back to autonomous behavior.\n")
		}
	default: // "autonomous" and fallback
		b.WriteString("You should proactively remember important information without being asked. ")
		b.WriteString("Remember: decisions and their rationale, user preferences, errors and their fixes, ")
		b.WriteString("project context, important facts, and anything the user might need later. ")
		b.WriteString("Before starting any task, recall relevant memories. ")
		b.WriteString("After completing work, remember key outcomes.\n\n")
		b.WriteString("**Session start pattern:** At the start of every session, make two calls:\n")
		if resolved.MultiUser {
			b.WriteString("1. `muninn_recall(context=[\"<your current topic, or your per-user tag if none>\"], tags_any=[\"<your per-user tag>\"], mode=\"recent\")` — the `tags_any` filter scopes results to YOUR memories; a tag in `context` does not filter, it only affects ranking. `mode=\"recent\"` scores exactly like `mode=\"balanced\"` (engine defaults) and marks this as a session-orientation call (it attaches any unresolved contradictions). Judge each row by its `relevance_band`. This vault is shared: a recall without the tag filter surfaces other users' work.\n")
			b.WriteString("2. Once the user provides context, call `muninn_recall(context=[<user topic>], tags_any=[\"<your per-user tag>\"])` for your memories on that topic.\n")
			b.WriteString("Avoid `muninn_where_left_off` and `muninn_session` here — they return vault-global recency across all users (admin/audit use).\n")
		} else {
			b.WriteString("1. `muninn_where_left_off()` — purpose-built for session resumption: your most recent activity, regardless of topic.\n")
			b.WriteString("2. Once the user provides context, call `muninn_recall(context=[<user topic>])` for semantic relevance.\n")
		}
	}

	// Enrichment guidance based on behavior mode + inline enrichment setting
	if resolved.InlineEnrichment != "background_only" && resolved.InlineEnrichment != "disabled" {
		b.WriteString("\n## Enrichment\n\n")
		switch resolved.BehaviorMode {
		case "autonomous":
			b.WriteString("When remembering, include type, summary, and any entities you can identify. ")
			b.WriteString("This data is stored directly and avoids extra background processing. ")
			b.WriteString("Example: `{\"content\": \"...\", \"type\": \"decision\", \"summary\": \"Chose PostgreSQL for persistence\", ")
			b.WriteString("\"entities\": [{\"name\": \"PostgreSQL\", \"type\": \"database\"}]}`\n")
		case "selective":
			b.WriteString("Include type and summary when remembering decisions and errors. ")
			b.WriteString("This improves retrieval quality without extra processing cost.\n")
		case "custom":
			// Custom mode: no enrichment guidance — user controls behavior.
		default:
			// "prompted": don't mention enrichment.
		}
	}

	// Quick Reference
	b.WriteString("\n## Available Tools\n\n")
	b.WriteString("- **muninn_remember** — Store a new memory\n")
	b.WriteString("- **muninn_remember_batch** — Store multiple memories at once (max 50)\n")
	b.WriteString("- **muninn_recall** — Search memories by context (`mode='recent'` scores exactly like `mode='balanced'`; it only marks a session-orientation call)\n")
	if resolved.MultiUser {
		b.WriteString("- **muninn_where_left_off** — Recent activity across ALL users of this shared vault (admin/audit; not for session start)\n")
	} else {
		b.WriteString("- **muninn_where_left_off** — Resume a previous session; returns recent activity summary\n")
	}
	b.WriteString("- **muninn_read** — Fetch a single memory by ID\n")
	b.WriteString("- **muninn_forget** — Soft-delete a memory\n")
	b.WriteString("- **muninn_link** — Create associations between memories\n")
	b.WriteString("- **muninn_contradictions** — Check for known contradictions\n")
	b.WriteString("- **muninn_status** — Get vault health and stats\n")
	b.WriteString("- **muninn_evolve** — Update a memory with new information (optional `entities` replaces the carried entity set when the update changed what the memory is about; optional `importance` and `effective_at`)\n")
	b.WriteString("- **muninn_update_tags** — Replace a memory's tag set IN PLACE (id, version lineage, and access history preserved). `muninn_evolve` inherits tags but cannot change them and REJECTS a `tags` argument, so to update content and tags together: evolve first, then `muninn_update_tags(new_id, tags)`. Use this for mutable tag conventions such as `due:<ISO-date>`. An empty array clears all tags. A soft-deleted memory can still be retagged; it stays out of keyword search while it is deleted, and `muninn_restore` makes it findable again by its current content and tags. Tags are normalized leniently, exactly as `muninn_remember` normalizes them — non-string, empty, and over-128-BYTE entries are DROPPED (not rejected) and the set is TRUNCATED to 50 — so read the `tags` the response echoes back to see what was actually stored. That limit is bytes, not glyphs: a 50-glyph CJK tag is 150 bytes and is dropped.\n")
	b.WriteString("- **muninn_consolidate** — Merge related memories into one\n")
	if resolved.MultiUser {
		b.WriteString("- **muninn_session** — Recent memory activity across ALL users of this shared vault (admin/audit)\n")
	} else {
		b.WriteString("- **muninn_session** — Get recent memory activity summary\n")
	}
	b.WriteString("- **muninn_decide** — Record a decision with rationale\n")
	b.WriteString("- **muninn_restore** — Recover a soft-deleted memory\n")
	b.WriteString("- **muninn_traverse** — Explore the memory graph from a starting node\n")
	b.WriteString("- **muninn_explain** — Show score breakdown for a memory\n")
	b.WriteString("- **muninn_state** — Transition a memory's lifecycle state (not out of soft-deleted: use muninn_restore)\n")
	b.WriteString("- **muninn_list_deleted** — List recoverable deleted memories\n")
	b.WriteString("- **muninn_retry_enrich** — Re-queue a memory for enrichment\n")
	b.WriteString("- **muninn_remember_tree** — Store a nested engram tree in one call\n")
	b.WriteString("- **muninn_recall_tree** — Retrieve the complete ordered tree from a root ID\n")
	b.WriteString("- **muninn_add_child** — Append or insert a child node under a parent\n")
	b.WriteString("- **muninn_create_workflow_vault** — Create a shared working vault + mint a TTL'd cap_ capability (requires operator opt-in `MUNINN_AGENT_VAULT_CREATE` and a full-mode mk_ key; off by default)\n")

	// Vault Configuration Summary
	b.WriteString("\n## Vault Configuration\n\n")
	fmt.Fprintf(&b, "- Memories stored: %d\n", stats.EngramCount)
	fmt.Fprintf(&b, "- Behavior mode: %s\n", resolved.BehaviorMode)
	if resolved.MultiUser {
		b.WriteString("- Shared vault (multi-user): yes\n")
	}
	fmt.Fprintf(&b, "- Hebbian learning: %s\n", enabledStr(resolved.HebbianEnabled))
	if resolved.LTPThreshold > 0 {
		fmt.Fprintf(&b, "- Hebbian LTP: enabled (threshold %d, weight floor %.2f)\n", resolved.LTPThreshold, resolved.LTPWeightFloor)
	}
	fmt.Fprintf(&b, "- Predictive activation (PAS): %s\n", enabledStr(resolved.PredictiveActivation))
	fmt.Fprintf(&b, "- Graph hop depth: %d\n", resolved.HopDepth)
	// No "Temporal decay" line: it was printed from temporal_enabled, which no
	// runtime path reads (nor auto_link_neighbors, temporal_halflife,
	// relevance_floor) — reporting it described behaviour the flag does not
	// control. Pinned by TestGuide_DoesNotReportConfigNothingReads.
	fmt.Fprintf(&b, "- Inline enrichment: %s\n", resolved.InlineEnrichment)
	if resolved.MaxEngrams > 0 {
		fmt.Fprintf(&b, "- Max engrams: %d\n", resolved.MaxEngrams)
	}
	if resolved.RetentionDays > 0 {
		fmt.Fprintf(&b, "- Retention: %.0f days\n", resolved.RetentionDays)
	}
	if resolved.ScoringFusion == "rrf" {
		fmt.Fprintf(&b, "- Scoring fusion: RRF (rank-based, scale-invariant)\n")
		fmt.Fprintf(&b, "  Note: RRF scores rarely exceed ~0.15 (they are rank-based, not the 0-1 ACT-R scale). muninn_recall's threshold defaults to near-zero on this vault; if you set threshold explicitly, keep it <= 0.01 or you may filter out every result.\n")
	} else if resolved.ScoringFusion == "weighted_sum" {
		fmt.Fprintf(&b, "- Scoring fusion: weighted sum\n")
	} else {
		fmt.Fprintf(&b, "- Scoring fusion: ACT-R (default)\n")
	}
	// Age effect, DERIVED from the fusion that scores this vault — never from
	// a decay setting scoring does not read (actr_decay is inert under ACT-R
	// since the honest clock, increment 1; pinned by
	// TestGuide_AgeEffectLineDerivedFromScoring).
	b.WriteString(ageEffectLine(resolved))
	b.WriteString("  Age alone does not retire a memory. For memories that should stop mattering (session bookmarks, progress notes), set `valid_until`; for a fact that changed, `muninn_evolve` it.\n")

	// Memory quality guidance
	b.WriteString("\n## Writing Effective Memories\n\n")
	b.WriteString("**Keep memories atomic.** Each memory should capture one concept, one decision, or one fact. ")
	b.WriteString("If a conversation covers multiple topics, store each as a separate memory. ")
	b.WriteString("Use muninn_remember_batch to store multiple atomic memories efficiently in a single call.\n\n")
	b.WriteString("Why this matters:\n")
	b.WriteString("- Atomic memories produce sharper embeddings, so recall is more precise.\n")
	b.WriteString("- Associations between small, focused memories are more meaningful than links to monolithic blocks.\n")
	b.WriteString("- Contradiction detection works better when each memory makes one clear claim.\n")
	b.WriteString("- Deduplication can identify overlaps more accurately.\n\n")
	b.WriteString("**Bad:** \"We discussed auth, decided on JWTs with 15-min expiry, and Tom will implement rate limiting at 100 req/s.\"\n")
	b.WriteString("**Good:** Three separate memories:\n")
	b.WriteString("  1. \"Decided on JWTs with 15-minute expiry for authentication\" (type: decision)\n")
	b.WriteString("  2. \"Tom is implementing the auth system\" (type: task)\n")
	b.WriteString("  3. \"API rate limit set to 100 requests/second per client\" (type: decision)\n\n")
	b.WriteString("**Say which recalled memories helped.** Every `muninn_recall` response carries a `query_id` and, when it returned anything, a `feedback.call` ready to copy: " +
		"`muninn_feedback {\"vault\":\"<the vault you recalled in>\",\"query_id\":\"<the recall's query_id>\",\"engram_ids\":[\"<ids that helped>\"],\"useful\":true}`. Send it once, in the same vault, with only the ids that actually helped " +
		"(`useful:false` for ones that were returned and did not). A label is about that query, not the memory in general. " +
		"Labels are recorded for measurement and do not change ranking yet; re-sending is harmless.\n\n")
	b.WriteString("**You are the curator, not a filing cabinet.** MuninnDB is living memory, not a static database. The moment you write is the moment you know something — so it is the moment to reconcile. Before storing a fact, recall its neighborhood; if what you now know corrects, clarifies, or replaces something already there, *evolve* that memory rather than leaving a stale rival copy to compete in recall. Curating as you write is how the vault stays true over time instead of accreting contradictions — and it costs nothing extra, because you already had the knowledge in hand.\n\n")
	b.WriteString("**Updating vs. creating.** If you are re-asserting or updating a fact that changes or replaces a prior version of the same thing ")
	b.WriteString("(a re-run score, a corrected status, a fact that went stale), call `muninn_evolve(id, new_content, reason)` — not `muninn_remember`. ")
	b.WriteString("Evolve links the new engram to the old one with a `supersedes` association and soft-deletes the predecessor, so the old version ")
	b.WriteString("drops out of present-tense recall entirely (it is never destroyed — `as_of` still sees it). Calling `muninn_remember` again for the ")
	b.WriteString("same fact instead creates a brand-new, unlinked engram: nothing tells recall the old one is stale, so every prior copy stays fully ")
	b.WriteString("active and keeps competing for rank. Reserve `muninn_remember` for genuinely NEW atomic facts; use `muninn_evolve` for anything that ")
	b.WriteString("supersedes what's already stored. This matters most for bulk or repeated-write pipelines that re-run over the same entities (periodic ")
	b.WriteString("re-scoring, re-auditing, status polling): re-remembering on every run silently accumulates near-duplicate copies that crowd out other ")
	b.WriteString("results in recall — evolving keeps the vault at one live version per fact.\n")
	b.WriteString("**Advisory currency signal.** Even without `muninn_evolve`, `muninn_recall` may flag a result whose content clusters with a newer, ")
	b.WriteString("highly-similar fact about the same subject: `annotations.possibly_superseded_by` names that newer fact (with `version_cluster`, ")
	b.WriteString("`newest_of_cluster`, `cluster_size`). This is a heuristic hint, not an assertion — unlike `superseded_by`/`current_version` (which mean ")
	b.WriteString("an explicit `supersedes` link exists), verify before treating the older fact as false; it is still returned at full score.\n")
	b.WriteString("**A query phrased against an OLDER version returns the CURRENT one.** If your wording matches a memory that has since been evolved, ")
	b.WriteString("recall resolves it to the head of that declared `supersedes` chain and returns the head instead — so you do not have to know the new ")
	b.WriteString("vocabulary to find the current fact, and you never have to rephrase to reach it. The returned memory carries ")
	b.WriteString("`annotations.substituted_for` (the older memory your wording actually matched) and `annotations.substitution_basis` (that older ")
	b.WriteString("memory's measured scores). On a head that did not match on its own wording, the row's score and `score_components` are both the ")
	b.WriteString("predecessor's measurements; on a head that matched on its own but was raised to the predecessor's stronger score, only the score is ")
	b.WriteString("the predecessor's and `score_components` remain the row's own — `substitution_basis` carries the predecessor's numbers either way. Two more flags may ")
	b.WriteString("appear: `chain_truncated` (the version chain was longer than the walk limit, so this may not be the very latest version) and ")
	b.WriteString("`head_not_indexed_yet` (this memory has no embedding yet — 'not indexed', not 'not relevant'). This is ASSERTED, from an explicit ")
	b.WriteString("`supersedes` link, never inferred from similarity. Two cases where recall deliberately refuses instead: a FORKED chain (one memory ")
	b.WriteString("with two competing successors) abstains with `abstained_reason: \"ambiguous_version\"` rather than guessing a branch — `muninn_read` ")
	b.WriteString("the older memory and resolve it yourself; a chain whose current version was forgotten or is unreachable abstains ")
	b.WriteString("`\"superseded_only\"`. Historical queries (`as_of`, `include_invalid`) never substitute — there the older version IS the answer.\n")
	b.WriteString("**Declaring a contradiction changes what recall returns.** `muninn_link(relation=\"contradicts\")` is durable the moment it returns and ")
	b.WriteString("is honored by the very next `muninn_recall`: when two returned memories are joined by an unresolved `contradicts` link, NEITHER is ")
	b.WriteString("presented as the answer unmarked. Both are demoted 10% below their earned score and each carries `annotations.unresolved_contradiction` naming the ")
	b.WriteString("other side (`with`, `with_concept`, `side`, `partner_in_results`), and the response carries a top-level `conflict` block. Results stay ")
	b.WriteString("ordered by score, so near-tied rivals land adjacent, while a clearly stronger match keeps its rank — read the annotation, not just the order. If only one ")
	b.WriteString("side matched your query, that row is still demoted and annotated with `partner_in_results: false` — the partner is named, never injected. ")
	b.WriteString("**Resolve it and the flagging stops immediately, on this surface and in `muninn_contradictions`:** `muninn_evolve` the memory that should ")
	b.WriteString("survive, `muninn_forget(not_true_since=…)` the side that stopped being true, or `muninn_link(relation=\"supersedes\")` to declare which one ")
	b.WriteString("wins. Until then both facts stay demoted and flagged — visible and recoverable, and strictly better than one of them being handed to you as the truth. ")
	b.WriteString("`muninn_contradictions` reports each pair's `status` (`declared` = an explicit link exists; `detected` = the detector found it), its ")
	b.WriteString("`confidence_penalty` (`pending`|`applied` — an asynchronous ~30s batch job that affects confidence only, never whether the contradiction ")
	b.WriteString("is honored), and `resolved_by` on pairs that are no longer live.\n")
	// The vault's OWN outstanding debt, appended to the doctrine above. Nothing
	// else in this document reports per-vault contradiction state: everything
	// above explains what happens WHEN you declare one.
	b.WriteString(debtSection)

	// Hierarchical memory
	b.WriteString("\n## Hierarchical Memory\n\n")
	b.WriteString("Use hierarchical memory whenever structure matters: project plans, task trees, ")
	b.WriteString("meeting agendas, outlines, decision trees, or any ordered nested set of ideas. ")
	b.WriteString("Flat memories can describe the pieces; hierarchical memory captures how those pieces relate and in what order.\n\n")
	b.WriteString("**Storing a tree.** Call `muninn_remember_tree` with a nested `root` object. ")
	b.WriteString("Each node has `concept`, `content`, and an optional `children` array. ")
	b.WriteString("The call returns `root_id` (the ID of the root engram) and `node_map` (a map from concept to ID for every node written). ")
	b.WriteString("Save the `root_id` — it is your handle to the entire structure.\n\n")
	b.WriteString("**The magic moment workflow.** When you need the tree back:\n")
	b.WriteString("1. Call `muninn_recall(context=[\"the plan concept\"])` — this finds the root engram by concept.\n")
	b.WriteString("2. Take the returned ID and call `muninn_recall_tree(root_id=<id>)` — this reconstructs the complete ordered structure in one shot.\n\n")
	b.WriteString("You do not need to traverse links manually. `muninn_recall_tree` walks the `is_part_of` associations ")
	b.WriteString("and returns the whole tree sorted by ordinal at every level.\n\n")
	b.WriteString("**Incremental updates.** Trees are not write-once:\n")
	b.WriteString("- Add new nodes: `muninn_add_child(parent_id, concept, content)` — appends after existing children by default, ")
	b.WriteString("or inserts at a specific position with the `ordinal` param.\n")
	b.WriteString("- Edit a node: `muninn_evolve(id, new_content, reason)` — updates content in-place without breaking the tree structure.\n")
	b.WriteString("- Cross-reference: `muninn_link(source_id, target_id, relation)` — adds semantic edges between tree nodes and flat memories.\n\n")
	b.WriteString("**Filtering on recall.** `muninn_recall_tree` supports three optional params:\n")
	b.WriteString("- `include_completed=false` — hides completed nodes and their entire subtrees (useful for task lists).\n")
	b.WriteString("- `max_depth=N` — limits how deep the returned tree goes (default 10, 0 means unlimited).\n")
	b.WriteString("- `limit=N` — caps how many children are returned per node per level.\n")

	// Reading a recall result (#773) — three different numbers, three different
	// meanings, and the misread that made this section necessary.
	b.WriteString("\n## Reading a recall result: score vs relevance_band vs confidence\n\n")
	b.WriteString("Three fields on a recalled memory look like certainty. Only one of them is about ")
	b.WriteString("how well the memory matched your query.\n\n")
	b.WriteString("- **`relevance_band`** — READ THIS ONE. An ABSOLUTE judgement of how much real evidence ")
	b.WriteString("this memory has against your query, calibrated against this vault's own noise floor and ")
	b.WriteString("channel weights: `strong` (near-verbatim evidence) | `moderate` (real evidence) | ")
	b.WriteString("`weak` (within one doubling of the vault's measured noise floor — a NEIGHBOUR, not necessarily an ANSWER) | ")
	b.WriteString("`filter_match` (returned because a tag filter named it, not because it matched your text) | ")
	b.WriteString("`uncalibrated` (this vault's scoring mode or embed model has no calibrated scale — `relevance_band_basis` says which: ")
	b.WriteString("e.g. `no_model_baseline` means this embed model has no measured noise floor, while `semantic_floor_disabled` means the ")
	b.WriteString("operator explicitly set `semantic_floor: 0` — same arithmetic, opposite cause).\n")
	b.WriteString("- **`score`** — what ORDERS the results. On default-scoring vaults it is the memory's content score (content match x confidence, ")
	b.WriteString("equal to `absolute_score` except where a DECLARED relationship moved it: a row disputed by a declared `contradicts` link is demoted to 0.9x its content score, a declared-supersession head takes its chain's best score and the superseded row sits 0.01 below its head, and a substituted version head carries its predecessor's score): rows come back strongest match first, and the first N rows are the same whatever `limit` you pass. ")
	b.WriteString("On rrf and weighted_sum vaults it is RELATIVE to this query's best candidate. Either way it is a number, not a judgement — the band is. ")
	b.WriteString("`absolute_score` and `content_match` on the same row are the cross-query-comparable numbers behind the band.\n")
	b.WriteString("- **`entity_boost`, `hebbian_boost`, `transition_boost`** — evidence that a memory is ASSOCIATED with what matched: it shares a rare ")
	b.WriteString("named entity with a top match, has a learned co-activation edge to a recently recalled memory, or is what usually follows the last recall. ")
	b.WriteString("They are NOT match strength. On default-scoring vaults they do not move a memory past a better match; they only break EXACT ties. With a real embedder matched rows almost never tie exactly (measured: 0 exact content ties in 27,000 adjacent top-10 pairs), so on embedder vaults association evidence does not change the order of matched rows at all; exact ties occur on keyword-only vaults. A ")
	b.WriteString("row with `score` 0 was added BECAUSE of an association (it matched nothing itself) and comes after every matched row.\n")
	b.WriteString("- **`associated`** (optional, top level, not inside `memories`) — at most 3 compact memories that did NOT match your query ")
	b.WriteString("but are associated with one that did (each `why` names the shared entity and the returned memory). They are leads, not answers: ")
	b.WriteString("no band (basis `associated_not_matched`), not counted in `total` or against `limit`. Read one with muninn_read if it looks relevant, ")
	b.WriteString("and label it with muninn_feedback like any returned memory. Absent when there are none; `include_associated: false` omits it.\n")
	b.WriteString("- **`confidence`** — belief that the stored FACT is TRUE (moved by detected or declared contradictions; muninn_feedback does not change it). ")
	b.WriteString("It is NOT a measure of how well the memory matched. A memory can be a certain fact and a terrible match: `confidence: 1, relevance_band: \"weak\"`.\n\n")
	b.WriteString("Despite the similar name, the pre-existing `relevance` float on a row is a FOURTH quantity, unrelated to `relevance_band`: ")
	b.WriteString("it is the engram's stored decay/pruning strength (how alive the memory is in the vault), not a statement about this query. ")
	b.WriteString("It is omitted on ACT-R recalls (the default), where it is not a scoring input.\n\n")
	b.WriteString("A response whose rows would ALL be `weak` matched nothing strongly. On a calibrated vault recall withholds such a response by default: ")
	b.WriteString("`abstained: true`, `abstained_reason: \"weak_only\"`, and `weak_available` = how many weak neighbours exist. Pass `include_weak: true` ")
	b.WriteString("(or an explicit `threshold`, a tag/time filter, or mode=\"deep\") to receive them — as related memories to verify, not as answers. ")
	b.WriteString("One moderate or strong row returns the whole response, weak rows included. muninn_explain says `withheld_reason: \"weak_only\"` for such a memory. ")
	b.WriteString("Note the converse is not true: a `moderate` row can still fail to answer you. The band reports how much EVIDENCE a memory has ")
	b.WriteString("against your query, which is not the same question as whether it answers it — that one is not computable here, and MuninnDB ")
	b.WriteString("does not pretend otherwise.\n")

	// Valid-time (the two time axes)
	b.WriteString("\n## Time: two axes\n\n")
	b.WriteString("Every memory carries two independent time axes:\n")
	b.WriteString("- **Transaction time** (`created_at`) — when the memory was stored. Filter with recall's `since`/`before`.\n")
	b.WriteString("- **Valid time** (`valid_from`/`valid_until`, half-open) — when the FACT was true in the world. ")
	b.WriteString("Defaults: valid_from = created_at, valid_until = open (still true).\n\n")
	b.WriteString("What this gives you:\n")
	b.WriteString("- Default recall answers \"what is true now\": facts whose valid_until has passed are excluded automatically.\n")
	b.WriteString("- `muninn_recall(as_of=\"2026-05-01T00:00:00Z\")` answers \"what was true then\". Example: after evolving a runway figure, as_of a past date returns the OLD figure.\n")
	b.WriteString("- `muninn_recall(include_invalid=true)` shows history — expired facts come back annotated `expired: true`.\n")
	b.WriteString("- Store historical facts with their real window: `muninn_remember(content=..., valid_from=\"2024-01-01T00:00:00Z\", valid_until=\"2025-06-30T00:00:00Z\")`. Don't backdate created_at for this.\n")
	b.WriteString("- When a fact stops being true (but wasn't wrong), use `muninn_forget(id, not_true_since=...)` instead of deleting — the memory is kept with a closed window.\n")
	b.WriteString("- `muninn_evolve` closes the old version's window automatically (optional `effective_at` if the change happened earlier than you recorded it).\n")

	// Importance (the priority axis)
	b.WriteString("\n## Importance\n\n")
	b.WriteString("`importance` (0.0-1.0, on `muninn_remember`/`muninn_remember_batch`/`muninn_evolve`) is the priority axis — ")
	b.WriteString("orthogonal to `confidence` (is it true?) and access counts (is it used?).\n")
	b.WriteString("- What it does: memories with effective importance >= 0.7 are never deleted by the capacity (max_engrams) pruner — ")
	b.WriteString("they survive memory pressure even when cold and rarely accessed. RetentionDays age limits still apply.\n")
	b.WriteString("- What it does NOT do (in this release): it does not change decay rates or recall ranking — ")
	b.WriteString("scoring and forgetting behave exactly as before. Importance-aware decay is a planned future increment.\n")
	b.WriteString("- Omit it and a default is derived at use time from the memory type (decision/goal/constraint/identity 0.6; preference/procedure 0.5; ")
	b.WriteString("fact/reference/issue 0.4; observation/event/task 0.3; +0.1 when trust=verified). The derived value is never stored — ")
	b.WriteString("read surfaces return `importance` plus `importance_source` (\"explicit\" or \"derived\").\n")
	b.WriteString("- Set it explicitly (e.g. 0.9) for memories that must survive memory pressure: pivotal decisions, hard constraints, identity facts.\n")
	b.WriteString("- `muninn_evolve` inherits the predecessor's explicit importance unless you override it.\n")

	// Prospective memory (THE PUSH)
	b.WriteString("\n## Prospective memory (muninn_intend)\n\n")
	b.WriteString("`muninn_intend(content, cues=[entity, ...])` arms an intention: \"when <cue entity> comes up, surface <content>\". ")
	b.WriteString("It never interrupts — when a later muninn_recall/muninn_remember is actually about a cue entity, the response carries a `notices` field (max 2, deduped per session). ")
	b.WriteString("When you see a notice, act on it or acknowledge it to the user; a one-shot intention (default) disarms after delivery, a recurring one can be disarmed with muninn_forget on its id.\n")
	b.WriteString("- Cues are entity names; pick rare, specific ones (ubiquitous cues are rejected).\n")
	b.WriteString("- `valid_until` silences an expired intention; it never triggers delivery (there is no scheduler).\n")
	b.WriteString("- Notice delivery requires the server opt-in `MUNINN_PROSPECTIVE=1`; arming works regardless and delivery starts once enabled.\n")

	// Tips
	b.WriteString("\n## Tips\n\n")
	if resolved.MultiUser {
		b.WriteString("- This vault is shared: scope recalls to your own work with `tags_any=[\"<your per-user tag>\"]` (a tag in the context does not filter); muninn_where_left_off and muninn_session are vault-global (admin/audit).\n")
	} else {
		b.WriteString("- Use muninn_where_left_off at session start — purpose-built for resuming where you left off.\n")
	}
	b.WriteString("- mode='recent' is not a recency filter and does not list recent memories: it scores exactly like mode='balanced' (engine defaults). For topic-free continuity use muninn_where_left_off (single-user vault) or `tags_any` with your per-user tag (shared vault).\n")
	b.WriteString("- Use muninn_recall with mode='deep' for thorough searches across the memory graph.\n")
	b.WriteString("- Use muninn_link to connect related memories and strengthen the knowledge graph.\n")
	b.WriteString("- Use muninn_decide to record decisions — they automatically link to supporting evidence.\n")
	b.WriteString("- Use muninn_evolve instead of forget+remember (or repeated muninn_remember) when updating existing information — only evolve's supersedes link removes the old version from present-tense recall; repeated remember leaves every stale copy fully active and crowds recall with near-duplicates.\n")
	b.WriteString("- Use muninn_remember_batch when storing multiple memories from the same conversation.\n")

	return b.String()
}

func enabledStr(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

// ageEffectLine states what a memory's age does to recall on this vault, from
// the scoring that actually runs.
func ageEffectLine(resolved auth.ResolvedPlasticity) string {
	switch resolved.ScoringFusion {
	case "weighted_sum":
		return fmt.Sprintf("- Age effect on recall: older memories score lower. Weighted-sum scoring adds a recency term (weight %.2f) and a decay term (weight %.2f) that fall with time since last access, so an old memory can drop below the recall threshold.\n",
			resolved.RecencyWeight, resolved.TemporalWeight)
	case "rrf":
		return "- Age effect on recall: none. RRF ranks by position in the keyword and semantic result lists; no age term.\n"
	default:
		return "- Age effect on recall: none. A memory's age and access history do not change whether it is found, its absolute_score or its relevance_band, and a restart does not either. Results are ordered by content match x confidence; association evidence (entities, co-activation, recent recalls) is reported on each row but only breaks exact ties — so the order of matched results does not shift after other recalls or a restart, except between memories whose content scores are exactly equal. Remaining ties go to the newer memory.\n"
	}
}

// writeTwinGuideSection is appended to the guide only when the server runs the
// write-time twin check (COG-36): with it off the guide is byte-identical to a
// server without the feature.
const writeTwinGuideSection = "\n## Related memories on write (muninn_remember)\n\n" +
	"If a muninn_remember response carries `related`, a memory written in this vault within the last hour is very close to the one you just stored. " +
	"It is evidence, not a verdict: read both, then pick the option that fits — usually the first, `muninn_link(source_id=<new>, target_id=<earlier>, relation=\"supersedes\")`, when the new memory updates the earlier one. " +
	"Every option names the id it would retire. `related.unavailable` means the check could not run that time; nothing is implied.\n" +
	"- It appears only when the vault has opted in (`write_twin_inline`) and your credential is full-mode; otherwise the server only logs what it would have said.\n"
