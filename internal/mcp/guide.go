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

func generateGuide(vaultName string, resolved auth.ResolvedPlasticity, stats engineStats) string {
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
		b.WriteString("**Session start pattern:** At the start of every session, call recall twice:\n")
		if resolved.MultiUser {
			b.WriteString("1. `muninn_recall(context=[\"<your per-user tag>\", \"session start\"], mode=\"recent\")` — restores YOUR recent continuity. This vault is shared: unscoped recency surfaces other users' work.\n")
			b.WriteString("2. Once the user provides context, call `muninn_recall(context=[<user topic>])` for semantic relevance.\n")
			b.WriteString("Avoid `muninn_where_left_off` and `muninn_session` here — they return vault-global recency across all users (admin/audit use).\n")
		} else {
			b.WriteString("1. `muninn_recall(context=[\"session start\"], mode=\"recent\")` — restores recent continuity regardless of topic.\n")
			b.WriteString("2. Once the user provides context, call `muninn_recall(context=[<user topic>])` for semantic relevance.\n")
			b.WriteString("Alternatively, use `muninn_where_left_off` — it is purpose-built for session resumption.\n")
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
	b.WriteString("- **muninn_recall** — Search memories by semantic context (use mode='recent' at session start)\n")
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
	b.WriteString("- **muninn_state** — Transition a memory's lifecycle state\n")
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
	fmt.Fprintf(&b, "- Temporal decay: %s\n", enabledStr(resolved.TemporalEnabled))
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
	b.WriteString("  3. \"API rate limit set to 100 requests/second per client\" (type: decision)\n")

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
		b.WriteString("- This vault is shared: scope recalls to your own work (include your per-user tag in the context); muninn_where_left_off and muninn_session are vault-global (admin/audit).\n")
	} else {
		b.WriteString("- Use muninn_where_left_off at session start — purpose-built for resuming where you left off.\n")
	}
	b.WriteString("- Use muninn_recall with mode='recent' when you need continuity but lack specific context.\n")
	b.WriteString("- Use muninn_recall with mode='deep' for thorough searches across the memory graph.\n")
	b.WriteString("- Use muninn_link to connect related memories and strengthen the knowledge graph.\n")
	b.WriteString("- Use muninn_decide to record decisions — they automatically link to supporting evidence.\n")
	b.WriteString("- Use muninn_evolve instead of forget+remember when updating existing information.\n")
	b.WriteString("- Use muninn_remember_batch when storing multiple memories from the same conversation.\n")

	return b.String()
}

func enabledStr(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}
