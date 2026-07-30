package mcp

func allToolDefinitions() []ToolDefinition {
	vaultProp := map[string]any{
		"type":        "string",
		"description": "Vault name to scope the operation (default: 'default'). Optional when authenticating via a vault-pinned mk_ key.",
	}
	// entityTypeEnum lists the 14 recognised entity types (the single source of
	// truth is validEntityTypes in handlers.go). Any other value is coerced to
	// "other" on every user-facing write path, so advertising the enum lets MCP
	// clients validate before sending. Keep this in sync with validEntityTypes.
	entityTypeEnum := []string{
		"person", "organization", "location", "concept", "technology",
		"project", "tool", "database", "service", "framework",
		"language", "product", "event", "other",
	}
	return []ToolDefinition{
		{
			Name:        "muninn_remember",
			Description: "Store a new piece of information (engram) in long-term memory. IMPORTANT: Keep each memory atomic — one concept, decision, or fact per memory. If a conversation covers multiple topics, use muninn_remember_batch to store them as separate memories. Atomic memories produce sharper recall, better associations, and more accurate contradiction detection. TIP: Provide ‘entities’ and ‘entity_relationships’ whenever you can identify them — this builds the knowledge graph immediately without requiring background enrichment. NOTE: If the exact same content already exists in the vault, the existing memory ID is returned instead of creating a duplicate. CAUTION: If this call is RE-ASSERTING or UPDATING a fact you already stored (a re-run score, a refreshed status), use muninn_evolve(id, ...) on the prior engram instead — calling muninn_remember repeatedly for the same evolving fact leaves every stale copy fully active and crowds recall with near-duplicates.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":       vaultProp,
					"content":     map[string]any{"type": "string", "description": "The information to remember."},
					"concept":     map[string]any{"type": "string", "description": "Short label for this memory."},
					"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional topic tags."},
					"confidence":  map[string]any{"type": "number", "description": "Confidence score 0.0-1.0 (default 1.0)."},
					"importance":  map[string]any{"type": "number", "description": "Importance 0.0-1.0 — how much this memory matters (priority axis, orthogonal to confidence/truth). Memories with effective importance >= 0.7 are protected from capacity (max_engrams) pruning; importance does not affect decay or recall ranking. Omit to use a default derived from the memory type (decisions/goals/constraints rank higher than observations/events)."},
					"created_at":  map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this memory was created (transaction time). Defaults to now."},
					"valid_from":  map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this fact BECAME TRUE (application time — distinct from created_at, which records when it was stored). Defaults to created_at. Use for historical facts, e.g. storing today that the office moved last January."},
					"valid_until": map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this fact STOPPED being true (exclusive — the window is [valid_from, valid_until)). Omit for facts that are still true. Facts with valid_until in the past are excluded from default recall; retrieve them with as_of or include_invalid."},
					"type":        map[string]any{"type": "string", "description": "Memory type — either a built-in name (fact, decision, observation, preference, issue, task, procedure, event, goal, constraint, identity, reference) or a free-form label (e.g. 'architectural_decision', 'coding_pattern'). Built-in names set the enum; free-form labels are stored as type_label with enum defaulting to 'fact'."},
					"type_label":  map[string]any{"type": "string", "description": "Explicit free-form type label (e.g. 'architectural_decision'). Overrides the label inferred from 'type'."},
					"trust":       map[string]any{"type": "string", "enum": []string{"verified", "inferred", "external", "untrusted"}, "description": "Provenance trust level. Default 'inferred' (all AI-generated content). 'verified' = human-confirmed/admin-certified and requires a write or full credential (rejected for observe credentials). 'external' = imported from another system; 'untrusted' = flagged unreliable."},
					"summary":     map[string]any{"type": "string", "description": "One-line summary of what this memory captures. Providing this skips background summarization."},
					"entities": map[string]any{
						"type":        "array",
						"description": "Entities mentioned in this memory. Providing these skips background entity extraction.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string", "description": "Entity name (e.g. 'PostgreSQL', 'Auth Service')."},
								"type": map[string]any{"type": "string", "enum": entityTypeEnum, "description": "Entity type. One of the 14 recognised types (e.g. 'database', 'service', 'person', 'project'); any other value is stored as 'other'."},
							},
							"required": []string{"name", "type"},
						},
					},
					"relationships": map[string]any{
						"type":        "array",
						"description": "Relationships to existing memories. Creates associations at write time.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"target_id": map[string]any{"type": "string", "description": "ID of the target memory (ULID)."},
								"relation":  map[string]any{"type": "string", "description": "Relationship type (e.g. 'depends_on', 'supports', 'contradicts')."},
								"weight":    map[string]any{"type": "number", "description": "Association weight 0.0-1.0 (default 0.9)."},
							},
							"required": []string{"target_id", "relation"},
						},
					},
					"entity_relationships": map[string]any{
						"type":        "array",
						"description": "Typed semantic relationships between named entities in this memory. Populates the entity knowledge graph directly — no LLM enrichment required. Example: [{\"from_entity\":\"PostgreSQL\",\"to_entity\":\"Redis\",\"rel_type\":\"caches_with\",\"weight\":0.9}]. Common rel_types: uses, depends_on, caches_with, manages, owns, contradicts, supports, extends, implements, belongs_to.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"from_entity": map[string]any{"type": "string", "description": "Source entity name (must match an entity in 'entities' or already known to the vault)."},
								"to_entity":   map[string]any{"type": "string", "description": "Target entity name."},
								"rel_type":    map[string]any{"type": "string", "description": "Relationship type (e.g. uses, depends_on, caches_with, manages, contradicts)."},
								"weight":      map[string]any{"type": "number", "description": "Confidence 0.0-1.0 (default 0.9)."},
							},
							"required": []string{"from_entity", "to_entity", "rel_type"},
						},
					},
					"op_id": map[string]any{
						"type":        "string",
						"description": "Optional idempotency key. If set and a receipt exists for this key, the cached engram ID is returned without re-creating.",
					},
					"embedding": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "number"},
						"description": "Optional pre-computed embedding vector (array of floats). When provided, the server skips its own embedding step and uses this vector directly. The dimension must match the vault's existing embedding dimension, or the call will be rejected. Omit to let the server embed via its configured provider.",
					},
				},
				"required": []string{"content"},
			},
		},
		{
			Name:        "muninn_remember_batch",
			Description: "Store multiple memories at once. More efficient than calling muninn_remember repeatedly. Maximum 50 per batch. Best practice: break complex topics into individual atomic memories — one concept, decision, or fact each. This produces sharper embeddings, better associations, and more accurate retrieval.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"memories": map[string]any{
						"type":        "array",
						"description": "Array of memories to store (max 50).",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content":     map[string]any{"type": "string", "description": "The information to remember."},
								"concept":     map[string]any{"type": "string", "description": "Short label for this memory."},
								"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional topic tags."},
								"confidence":  map[string]any{"type": "number", "description": "Confidence score 0.0-1.0 (default 1.0)."},
								"importance":  map[string]any{"type": "number", "description": "Importance 0.0-1.0 (priority axis; >= 0.7 is protected from capacity pruning; does not affect decay or ranking). Omit for a type-derived default."},
								"created_at":  map[string]any{"type": "string", "description": "ISO 8601 timestamp (transaction time). Defaults to now."},
								"valid_from":  map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this fact became true (application time). Defaults to created_at."},
								"valid_until": map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this fact stopped being true (exclusive). Omit for facts still true."},
								"type":        map[string]any{"type": "string", "description": "Memory type — built-in name or free-form label."},
								"type_label":  map[string]any{"type": "string", "description": "Explicit free-form type label."},
								"trust":       map[string]any{"type": "string", "enum": []string{"verified", "inferred", "external", "untrusted"}, "description": "Provenance trust level. Default 'inferred'. 'verified' requires a write or full credential."},
								"summary":     map[string]any{"type": "string", "description": "One-line summary. Skips background summarization."},
								"entities": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"name": map[string]any{"type": "string"},
											"type": map[string]any{"type": "string"},
										},
										"required": []string{"name", "type"},
									},
									"description": "Entities mentioned in this memory.",
								},
								"relationships": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"target_id": map[string]any{"type": "string"},
											"relation":  map[string]any{"type": "string"},
											"weight":    map[string]any{"type": "number"},
										},
										"required": []string{"target_id", "relation"},
									},
									"description": "Relationships to existing memories.",
								},
								"entity_relationships": map[string]any{
									"type": "array",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"from_entity": map[string]any{"type": "string"},
											"to_entity":   map[string]any{"type": "string"},
											"rel_type":    map[string]any{"type": "string"},
											"weight":      map[string]any{"type": "number"},
										},
										"required": []string{"from_entity", "to_entity", "rel_type"},
									},
									"description": "Typed entity-to-entity relationships for this memory.",
								},
								"embedding": map[string]any{
									"type":        "array",
									"items":       map[string]any{"type": "number"},
									"description": "Optional pre-computed embedding vector for this memory. Must match the vault's embedding dimension.",
								},
							},
							"required": []string{"content"},
						},
					},
				},
				"required": []string{"memories"},
			},
		},
		{
			Name:        "muninn_recall",
			Description: "Search long-term memory using semantic context. Returns the most relevant memories.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"context":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Search context phrases."},
					"threshold": map[string]any{"type": "number", "description": "Minimum relevance score 0.0-1.0. Default is mode-aware: 0.5 for the normal (ACT-R) scoring vault; for a vault configured with scoring_fusion='rrf', scores are rank-based and rarely exceed ~0.15, so the default there is effectively 0 (near-zero floor) — an explicit threshold above ~0.01 on an rrf vault can filter out everything."},
					"limit":     map[string]any{"type": "integer", "description": "Max results to return (default 10)."},
					"profile": map[string]any{
						"type":        "string",
						"description": "Traversal profile for BFS graph traversal. Leave unset for automatic inference from your context phrases.\n• default       — balanced retrieval across all edge types; contradiction edges dampened (0.3×)\n• causal        — follow cause/effect/dependency chains (Causes, DependsOn, Blocks, PrecededBy, FollowedBy)\n• confirmatory  — find supporting evidence; contradiction edges excluded (Supports, Implements, Refines, References)\n• adversarial   — surface conflicts and contradictions (Contradicts, Supersedes, Blocks; Contradicts boosted 1.5×)\n• structural    — follow project/person/hierarchy edges (IsPartOf, BelongsToProject, CreatedByPerson)\n\nWhen to specify explicitly:\n  Use 'causal' when asking why something happened or what something depends on.\n  Use 'adversarial' when auditing for inconsistencies or contradictions.\n  Use 'confirmatory' when looking for supporting evidence for a claim.\n  Use 'structural' when navigating project or organizational structure.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"semantic", "recent", "balanced", "deep"},
						"description": "Recall mode preset.\n• semantic  — high-precision vector search (threshold=0.3)\n• recent    — recency-biased, 1 hop (threshold=0.2)\n• balanced  — engine defaults (no override)\n• deep      — exhaustive graph traversal, 4 hops (threshold=0.1)\nPreset thresholds are ACT-R-calibrated and apply only under ACT-R/weighted-sum scoring; on an rrf-fusion vault the preset threshold abstains and the rrf mode-aware default (~0.001) applies, so modes are safe to use there. An explicit threshold always wins.",
					},
					"since": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp (e.g. 2026-01-15T00:00:00Z). Only return memories CREATED after this time (transaction axis — when they were stored). For 'what was true at T', use as_of instead.",
					},
					"before": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp (e.g. 2026-01-20T00:00:00Z). Only return memories CREATED before this time (transaction axis).",
					},
					"as_of": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp. Time-travel on the VALIDITY axis: only return facts whose validity window [valid_from, valid_until) covers this moment — 'what was true at T', regardless of when it was stored. Default (omitted): 'what is true now' — facts whose valid_until has passed are excluded.",
					},
					"include_invalid": map[string]any{
						"type":        "boolean",
						"description": "When true, disables the validity gate: expired facts (valid_until <= now) are returned too, annotated with expired=true and their valid_until. Use to show history. Default false.",
					},
					"tags_all": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Only return memories carrying ALL of these tags (exact match, AND).",
					},
					"tags_any": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Only return memories carrying AT LEAST ONE of these tags (exact match, OR).",
					},
					"tag_filter": map[string]any{
						"type":        "object",
						"description": "Filter by a key:value tag convention via lexical comparison of the value after a prefix. Example: {\"prefix\":\"due:\",\"lte\":\"2026-06-17\"} matches memories tagged due:<date> where date <= 2026-06-17 (ISO dates sort lexically). A memory matches if any of its tags with that prefix satisfies the bound.",
						"properties": map[string]any{
							"prefix": map[string]any{"type": "string", "description": "Tag key prefix to match, e.g. \"due:\" or \"status:\"."},
							"lte":    map[string]any{"type": "string", "description": "Value (after prefix) must be <= this."},
							"gte":    map[string]any{"type": "string", "description": "Value must be >= this."},
							"lt":     map[string]any{"type": "string", "description": "Value must be < this."},
							"gt":     map[string]any{"type": "string", "description": "Value must be > this."},
							"eq":     map[string]any{"type": "string", "description": "Value must equal this."},
						},
						"required": []string{"prefix"},
					},
					"embedding": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "number"},
						"description": "Optional pre-computed query embedding vector (array of floats). When provided, the server uses this vector for semantic search instead of computing one from 'context'. The dimension must match the vault's existing embedding dimension, or the call will be rejected.",
					},
					"annotate": map[string]any{
						"type":        "boolean",
						"description": "When true, each result includes an annotations object with staleness, conflict, and supersession metadata. Default false. Independent of annotate: whenever a result belongs to a detected same-subject version cluster, annotations also carries an ADVISORY (never asserted) possibly_superseded_by/version_cluster/newest_of_cluster/cluster_size signal — a mechanical hint pointing an older cluster member at the newest, distinct from the authoritative superseded_by/current_version pair.",
					},
					"caller": map[string]any{
						"type":        "string",
						"description": "Your ownership-lease identity (conventionally '{host}:{session}'). Memories checked out by a live lease owned by someone else are hidden; your own leased memories are returned normally. See muninn_claim.",
					},
					"include_leased": map[string]any{
						"type":        "boolean",
						"description": "When true, disables work-queue lease filtering so memories checked out by other owners are also returned (admin/debugging). Default false.",
					},
					"read_only": map[string]any{
						"type":        "boolean",
						"description": "When true, marks this recall as a pure read that must not trigger any write side effects. Always effectively true for observe-mode credentials -- passing read_only=false with an observe credential is rejected. Default false.",
					},
				},
				"required": []string{"context"},
			},
		},
		{
			Name:        "muninn_read",
			Description: "Fetch a single memory by its ID. Returns full content plus any caller-provided entities (name, type) and entity relationships (from_entity, to_entity, rel_type) that were stored with the memory. Engine-generated co-occurrence data is excluded; use muninn_entity for that.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "Memory ID (ULID)."},
					"read_only": map[string]any{
						"type":        "boolean",
						"description": "When true, this read must not trigger reinforcement (AccessCount/LastAccess bump) or implicit feedback side effects. Always effectively true for observe-mode credentials -- passing read_only=false with an observe credential is rejected. Default false.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "muninn_forget",
			Description: "Soft-delete a memory (excluded from recall, recoverable for 7 days). If the memory isn't wrong but simply STOPPED BEING TRUE, pass not_true_since instead: the memory is then invalidated on the validity axis (kept, not deleted) and remains retrievable via recall's as_of/include_invalid.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "Memory ID to forget."},
					"not_true_since": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp. Instead of deleting, records that the fact stopped being true at this moment (sets valid_until). The memory stays active but drops out of default recall; as_of before this time still returns it.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "muninn_link",
			Description: "Create or strengthen an association between two memories.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"source_id": map[string]any{"type": "string", "description": "Source memory ID."},
					"target_id": map[string]any{"type": "string", "description": "Target memory ID."},
					"relation": map[string]any{
						"type":        "string",
						"description": "Type of relationship between the two memories. Choose the most specific type:\n• supports          — this memory provides evidence or backing for the other\n• contradicts       — this memory conflicts with or refutes the other\n• depends_on        — this memory requires the other to be understood or true first\n• supersedes        — this memory replaces or updates the other (other is now outdated)\n• relates_to        — general association when no specific type fits (safe default)\n• is_part_of        — this memory is a component or section of the other\n• causes            — this memory is a cause or contributing factor to the other\n• preceded_by       — this memory chronologically follows the other\n• followed_by       — this memory chronologically precedes the other\n• created_by_person — this memory was authored or owned by the person in the other\n• belongs_to_project — this memory belongs to the project or context in the other\n• references        — this memory cites or links to the other without strong semantic weight\n• implements        — this memory is the concrete realization of the other (e.g. code for a spec)\n• blocks            — this memory is an obstacle preventing progress on the other\n• resolves          — this memory is the solution or fix for the other\n• refines           — this memory is a near-duplicate refinement or correction of the other",
					},
					"weight": map[string]any{"type": "number", "description": "Association weight 0.0-1.0 (default 0.8)."},
				},
				"required": []string{"source_id", "target_id", "relation"},
			},
		},
		{
			Name:        "muninn_contradictions",
			Description: "Check for known contradictions in this vault.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_status",
			Description: "Get health and capacity statistics for the vault.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_evolve",
			Description: "Update a memory with new information. Creates a new version linked to the old one by a supersedes association, and soft-deletes the old version so it drops out of present-tense recall (never destroyed — as_of still sees it). Use this — not a repeated muninn_remember — whenever a new call is re-asserting or replacing a fact you already stored; otherwise every stale copy stays fully active and crowds recall with near-duplicates.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":       vaultProp,
					"id":          map[string]any{"type": "string", "description": "ID of the memory to evolve."},
					"new_content": map[string]any{"type": "string", "description": "Updated information."},
					"reason":      map[string]any{"type": "string", "description": "Why this memory is being updated."},
					"concept": map[string]any{
						"type":        "string",
						"description": "Optional new label for the memory. When omitted the concept is inherited verbatim. Use this to correct concepts that encode mutable state (e.g. change \"answer owed\" to \"answer sent — closed\").",
					},
					"effective_at": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp for when the new version BECAME TRUE (application time; defaults to now). The old version's validity window closes at this moment and the new version's opens — use when the change happened before you recorded it.",
					},
					"importance": map[string]any{
						"type":        "number",
						"description": "Importance 0.0-1.0 for the new version. Omit to inherit the predecessor's explicitly asserted importance (an unset predecessor stays unset and keeps its type-derived default).",
					},
					"embedding": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "number"},
						"description": "Optional pre-computed embedding vector for the new version. When provided, the server skips its own embedding step. Must match the vault's existing embedding dimension.",
					},
					"entities": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string"},
								"type": map[string]any{"type": "string", "description": "One of: person, organization, location, product, concept, event, other."},
							},
							"required": []string{"name", "type"},
						},
						"description": "Optional replacement entities for the new version, for when the update changes what the memory is about. When omitted, the predecessor's entity links carry forward unchanged. Maximum 20.",
					},
				},
				"required": []string{"id", "new_content", "reason"},
			},
		},
		{
			Name:        "muninn_consolidate",
			Description: "Merge multiple related memories into one. Archives the originals. Maximum 50 IDs.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":          vaultProp,
					"ids":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "IDs of memories to merge (max 50)."},
					"merged_content": map[string]any{"type": "string", "description": "Content for the consolidated memory."},
				},
				"required": []string{"ids", "merged_content"},
			},
		},
		{
			Name:        "muninn_session",
			Description: "Get a summary of recent memory activity since a timestamp — vault-wide: in a vault shared by multiple users or agents this includes other users' activity (admin/audit use there).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"since": map[string]any{"type": "string", "description": "ISO 8601 timestamp. Return activity after this time."},
				},
				"required": []string{"since"},
			},
		},
		{
			Name:        "muninn_decide",
			Description: "Record a decision with rationale and link it to supporting evidence.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":        vaultProp,
					"decision":     map[string]any{"type": "string", "description": "The decision made."},
					"rationale":    map[string]any{"type": "string", "description": "Reasoning behind the decision."},
					"alternatives": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Other options that were considered."},
					"evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Memory IDs that support this decision."},
				},
				"required": []string{"decision", "rationale"},
			},
		},
		// Epic 18: tools 12-17
		{
			Name:        "muninn_restore",
			Description: "Recover a soft-deleted memory within the 7-day recovery window. Use when you realize a memory was deleted by mistake. Returns the restored memory's state.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "ID of the deleted memory to restore."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "muninn_traverse",
			Description: "Explore the memory graph by following associations from a starting memory. Use when you want to discover related memories structurally rather than by semantic search. Returns nodes and edges within the specified hop distance.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":           vaultProp,
					"start_id":        map[string]any{"type": "string", "description": "ID of the memory to start from."},
					"max_hops":        map[string]any{"type": "integer", "description": "Maximum BFS depth from the starting node (default 2, max 5)."},
					"max_nodes":       map[string]any{"type": "integer", "description": "Maximum number of memories to return (default 20, max 100)."},
					"rel_types":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional: filter to specific relation types (e.g. [\"depends_on\", \"supports\"])."},
					"follow_entities": map[string]any{"type": "boolean", "description": "When true, the BFS also traverses through shared entity links (e.g. two memories that both mention 'PostgreSQL' are connected even without a direct association). Entity-hop edges are assigned a lower weight (0.1) than direct association edges. Default false."},
				},
				"required": []string{"start_id"},
			},
		},
		{
			Name:        "muninn_explain",
			Description: "Show the full score breakdown for why a specific memory would be returned for a given query. Use for debugging recall quality — to understand why a memory ranked high or low.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"engram_id": map[string]any{"type": "string", "description": "ID of the memory to score-explain."},
					"query":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Context phrases to evaluate against (same format as muninn_recall context)."},
					"embedding": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "number"},
						"description": "Optional pre-computed query embedding vector. When provided, used for the semantic similarity component instead of embedding the query strings server-side. Required for accurate semantic scores in zero-config mode.",
					},
				},
				"required": []string{"engram_id", "query"},
			},
		},
		{
			Name:        "muninn_state",
			Description: "Transition a memory's lifecycle state. Use to mark work as active, completed, paused, blocked, or archived. Valid states: planning, active, paused, blocked, completed, cancelled, archived.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":  vaultProp,
					"id":     map[string]any{"type": "string", "description": "ID of the memory to update."},
					"state":  map[string]any{"type": "string", "enum": []string{"planning", "active", "paused", "blocked", "completed", "cancelled", "archived"}, "description": "The new lifecycle state."},
					"reason": map[string]any{"type": "string", "description": "Optional: why the state is being changed."},
				},
				"required": []string{"id", "state"},
			},
		},
		{
			Name:        "muninn_compare_and_set",
			Description: "Atomically transition a memory's lifecycle state only if it currently matches an expected state (compare-and-set). Use to avoid clobbering concurrent transitions. Returns whether it applied and the current state/owner on conflict.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":        vaultProp,
					"id":           map[string]any{"type": "string", "description": "ID of the memory to update."},
					"expect_state": map[string]any{"type": "string", "enum": []string{"planning", "active", "paused", "blocked", "completed", "cancelled", "archived"}, "description": "Only apply if the current state equals this. Omit to skip the guard."},
					"set_state":    map[string]any{"type": "string", "enum": []string{"planning", "active", "paused", "blocked", "completed", "cancelled", "archived"}, "description": "The new lifecycle state to set when the guard holds."},
				},
				"required": []string{"id", "set_state"},
			},
		},
		{
			Name:        "muninn_claim",
			Description: "Atomically claim an advisory ownership lease on a memory so a fleet of agents can treat vault memories as a work queue and avoid double-processing the same item. Returns status acquired (was free), refreshed (already yours), reclaimed (took over a stale lease) or conflict (a live foreign owner holds it). A live foreign lease is never overwritten.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":    vaultProp,
					"id":       map[string]any{"type": "string", "description": "ID of the memory to claim."},
					"owner":    map[string]any{"type": "string", "description": "Stable holder identity, unique across hosts and sessions, conventionally '{host}:{session}'."},
					"ttl_secs": map[string]any{"type": "integer", "description": "Lease duration in seconds. The lease goes stale once this elapses without a refresh; pick a value that fits the unit of work."},
				},
				"required": []string{"id", "owner", "ttl_secs"},
			},
		},
		{
			Name:        "muninn_release",
			Description: "Release an ownership lease held by owner, making the memory immediately visible to recall again without waiting for the TTL. Idempotent: releasing an unleased memory, or one held by someone else, is a no-op.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "ID of the memory to release."},
					"owner": map[string]any{"type": "string", "description": "The holder identity used when the lease was claimed."},
				},
				"required": []string{"id", "owner"},
			},
		},
		{
			Name:        "muninn_list_deleted",
			Description: "List soft-deleted memories that are still within the 7-day recovery window. Use before calling muninn_restore to find what can be recovered.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"limit": map[string]any{"type": "integer", "description": "Max results to return (default 20, max 100)."},
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_retry_enrich",
			Description: "Re-queue a memory for enrichment processing by active plugins (e.g. embedding or LLM summarization) that have not yet completed. Use when a memory was stored before a plugin was activated.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "ID of the memory to re-enrich."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "muninn_get_enrichment_candidates",
			Description: "Return active memories that are missing one or more enrichment stages so an external MCP agent can enrich them without using the server-side enrich plugin.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"stages": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "enum": []string{"entities", "relationships", "classification", "summary"}},
						"description": "Which enrichment stages to look for. Defaults to all four stages.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of candidate memories to return in this call (default 50, max 200).",
					},
					"cursor": map[string]any{
						"type":        "string",
						"description": "Opaque pagination cursor returned by a previous call as next_cursor. Omit or pass an empty string to start from the beginning.",
					},
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_apply_enrichment",
			Description: "Persist externally generated enrichment output for a single memory. Use this after an MCP agent reads candidates, generates summary/entities/relationships itself, and writes results back without relying on the server-side enrich plugin.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":               vaultProp,
					"id":                  map[string]any{"type": "string", "description": "ID of the memory to update."},
					"expected_updated_at": map[string]any{"type": "string", "description": "RFC3339Nano timestamp from the candidate response. Prevents stale overwrites."},
					"summary":             map[string]any{"type": "string", "description": "Optional generated summary."},
					"memory_type":         map[string]any{"type": "string", "description": "Optional generated memory type."},
					"type_label":          map[string]any{"type": "string", "description": "Optional generated free-form type label."},
					"entities": map[string]any{
						"type":        "array",
						"description": "Optional extracted entities to persist.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":       map[string]any{"type": "string"},
								"type":       map[string]any{"type": "string", "enum": entityTypeEnum, "description": "Entity type. One of the 14 recognised types; any other value is stored as 'other'."},
								"confidence": map[string]any{"type": "number"},
							},
							"required": []string{"name", "type"},
						},
					},
					"relationships": map[string]any{
						"type":        "array",
						"description": "Optional extracted entity relationships to persist.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"from_entity": map[string]any{"type": "string"},
								"to_entity":   map[string]any{"type": "string"},
								"rel_type":    map[string]any{"type": "string"},
								"weight":      map[string]any{"type": "number"},
							},
							"required": []string{"from_entity", "to_entity", "rel_type"},
						},
					},
					"stages_completed": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "enum": []string{"entities", "relationships", "classification", "summary"}},
						"description": "Optional explicit stage list to mark complete even when the generated output for a stage is empty.",
					},
					"source": map[string]any{"type": "string", "description": "Optional provenance/source label for the applied enrichment (default: mcp_agent)."},
				},
				"required": []string{"id", "expected_updated_at"},
			},
		},
		{
			Name:        "muninn_guide",
			Description: "Get instructions on how to use MuninnDB effectively. Call this when you first connect or need a reminder of available capabilities and best practices.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_where_left_off",
			Description: "Surface what was being worked on at the end of the last session. Returns the most recently accessed active memories, sorted by recency — vault-wide: in a vault shared by multiple users or agents this includes other users' activity, so prefer a tag-scoped muninn_recall there. In single-user vaults, call at session start to orient yourself before any user queries.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max memories to return (default 10, max 50).",
					},
					"read_only": map[string]any{
						"type":        "boolean",
						"description": "When true, marks this call as a pure read. where_left_off never has write side effects regardless of this flag; it exists for API consistency with muninn_recall/muninn_read. Always effectively true for observe-mode credentials -- passing read_only=false with an observe credential is rejected. Default false.",
					},
					"exclude_type_labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Opt-in: type_label values to skip (e.g. \"session-log\"), so recency scan noise doesn't crowd out real memories. Excluded entries don't count against limit — the scan keeps going to fill it. Default: empty (no exclusion, all types included).",
					},
				},
				"required": []string{},
			},
		},
		// Entity reverse index tool
		{
			Name:        "muninn_find_by_entity",
			Description: "Return all memories that mention a given named entity. Uses the entity reverse index for fast O(matches) lookup. When the exact name has no matches, vault entity names are fuzzy-matched by token overlap (case/articles/separators ignored, e.g. 'knock' finds 'The Knock') and the response reports the resolution via matched_entity + fuzzy.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity_name": map[string]any{"type": "string", "description": "The entity name to look up (e.g. 'PostgreSQL', 'Alice')"},
					"vault":       vaultProp,
					"limit":       map[string]any{"type": "integer", "description": "Max results (1-50, default 20)"},
				},
				"required": []string{"entity_name"},
			},
		},
		// Entity lifecycle state tool
		{
			Name:        "muninn_entity_state",
			Description: "Set the lifecycle state of a named entity (active, deprecated, merged, resolved) and optionally correct its type. For state=merged, provide merged_into with the canonical entity name. The type field is optional — omit it to preserve the existing type.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity_name": map[string]any{"type": "string", "description": "The entity name to update"},
					"state":       map[string]any{"type": "string", "description": "New state: active, deprecated, merged, or resolved"},
					"merged_into": map[string]any{"type": "string", "description": "Canonical entity name (required when state=merged)"},
					"type":        map[string]any{"type": "string", "enum": entityTypeEnum, "description": "Correct the entity type to one of the 14 recognised types (e.g. 'concept', 'technology', 'product'). Any other value is stored as 'other'. Omit to preserve the existing type."},
					"vault":       vaultProp,
				},
				"required": []string{"entity_name", "state"},
			},
		},
		// Batch entity lifecycle state tool
		{
			Name:        "muninn_entity_state_batch",
			Description: "Update lifecycle state (and optionally type) for multiple entities in one call. More efficient than calling muninn_entity_state repeatedly. Maximum 50 per batch. Partial success supported — check per-item status in results.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"operations": map[string]any{
						"type":        "array",
						"description": "Array of entity state operations (max 50).",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"entity_name": map[string]any{"type": "string", "description": "Entity name to update"},
								"state":       map[string]any{"type": "string", "description": "New state: active, deprecated, merged, or resolved"},
								"merged_into": map[string]any{"type": "string", "description": "Canonical entity name (required when state=merged)"},
								"type":        map[string]any{"type": "string", "enum": entityTypeEnum, "description": "Correct the entity type to one of the 14 recognised types (e.g. 'concept', 'technology', 'product'). Any other value is stored as 'other'. Omit to preserve existing."},
							},
							"required": []string{"entity_name", "state"},
						},
					},
				},
				"required": []string{"operations"},
			},
		},
		// Hierarchical memory tools
		{
			Name:        "muninn_remember_tree",
			Description: "Store a nested hierarchy (project plan, task tree, outline) as a collection of linked engrams. Each node becomes a full engram with cognitive properties. Children are ordered by their position in the tree. Returns root_id and a node_map for future reference.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"root": map[string]any{
						"type":        "object",
						"description": "The root node of the tree. Each node may have a 'children' array for nesting.",
						"properties": map[string]any{
							"concept": map[string]any{"type": "string", "description": "Short label for this node."},
							"content": map[string]any{"type": "string", "description": "Content for this node."},
							"type":    map[string]any{"type": "string", "description": "Memory type (goal, task, etc.)."},
							"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"children": map[string]any{
								"type":        "array",
								"description": "Child nodes (same schema, recursive).",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"concept": map[string]any{"type": "string", "description": "Short label for this node."},
										"content": map[string]any{"type": "string", "description": "Content for this node."},
										"type":    map[string]any{"type": "string", "description": "Memory type (goal, task, etc.)."},
										"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
										"children": map[string]any{
											"type":        "array",
											"description": "Child nodes (recursive).",
											"items": map[string]any{
												"type":        "object",
												"description": "Nested child node.",
												"properties": map[string]any{
													"concept": map[string]any{"type": "string", "description": "Short label."},
													"content": map[string]any{"type": "string", "description": "Content."},
													"type":    map[string]any{"type": "string", "description": "Memory type."},
													"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
													"children": map[string]any{
														"type":        "array",
														"description": "Further nested children.",
														"items": map[string]any{
															"type":        "object",
															"description": "Deeply nested child node.",
															"properties": map[string]any{
																"concept": map[string]any{"type": "string"},
																"content": map[string]any{"type": "string"},
																"type":    map[string]any{"type": "string"},
																"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
																"children": map[string]any{
																	"type":        "array",
																	"description": "Deeper nesting - allows arbitrary depth.",
																	"items":       map[string]any{},
																},
															},
														},
													},
												},
											},
										},
									},
									"required": []string{"concept", "content"},
								},
							},
						},
						"required": []string{"concept", "content"},
					},
				},
				"required": []string{"root"},
			},
		},
		{
			Name:        "muninn_recall_tree",
			Description: "Retrieve the complete, ordered hierarchy rooted at root_id. Returns all nodes in their original structured order, with state and metadata at each level. Use after muninn_recall finds the root engram's ID.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":             vaultProp,
					"root_id":           map[string]any{"type": "string", "description": "ULID of the root engram."},
					"max_depth":         map[string]any{"type": "integer", "description": "Maximum recursion depth. 0 = unlimited (default: 10)."},
					"limit":             map[string]any{"type": "integer", "description": "Max children per node per level. 0 = no limit (default: 0)."},
					"include_completed": map[string]any{"type": "boolean", "description": "Include completed nodes and their subtrees (default: true)."},
				},
				"required": []string{"root_id"},
			},
		},
		// Entity cluster detection
		{
			Name:        "muninn_entity_clusters",
			Description: "Return entity pairs that frequently co-occur in the same memories. Uses the co-occurrence index for fast O(pairs) lookup. Useful for discovering implicit relationships between entities.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"min_count": map[string]any{"type": "integer", "description": "Minimum co-occurrence count to include a pair (default 2)."},
					"top_n":     map[string]any{"type": "integer", "description": "Maximum number of entity pairs to return, sorted by count descending (default 20)."},
				},
				"required": []string{},
			},
		},
		// Knowledge graph export
		{
			Name:        "muninn_export_graph",
			Description: "Export the entity relationship graph for a vault as JSON-LD or GraphML. Nodes are named entities; edges are typed entity-to-entity relationships extracted from memories. Useful for visualisation, graph analysis, or knowledge-base integration.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":           vaultProp,
					"format":          map[string]any{"type": "string", "enum": []string{"json-ld", "graphml"}, "description": "Output format: 'json-ld' (default) or 'graphml'."},
					"include_engrams": map[string]any{"type": "boolean", "description": "When true, entity types are enriched from the entity record table (default false)."},
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_add_child",
			Description: "Add a single child node to an existing parent in a tree. Writes the engram and wires the is_part_of association and ordinal key. Use for incremental tree updates without resending the whole tree.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"parent_id": map[string]any{"type": "string", "description": "ULID of the parent engram."},
					"concept":   map[string]any{"type": "string", "description": "Short label for the new child."},
					"content":   map[string]any{"type": "string", "description": "Content for the new child."},
					"type":      map[string]any{"type": "string", "description": "Memory type (task, goal, etc.)."},
					"tags":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"ordinal":   map[string]any{"type": "integer", "description": "Explicit ordinal position. Omit to append at end."},
					"embedding": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "number"},
						"description": "Optional pre-computed embedding vector for this child. Must match the vault's existing embedding dimension.",
					},
				},
				"required": []string{"parent_id", "concept", "content"},
			},
		},
		// Entity similarity detection and merge
		{
			Name:        "muninn_similar_entities",
			Description: "Find entity names in a vault that are likely duplicates based on trigram similarity. Returns pairs of similar names that may need merging. Use muninn_merge_entity to merge confirmed duplicates.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"threshold": map[string]any{"type": "number", "description": "Minimum similarity score 0.0-1.0 to include a pair (default 0.85)."},
					"top_n":     map[string]any{"type": "integer", "description": "Maximum number of similar pairs to return, sorted by similarity descending (default 20)."},
				},
				"required": []string{},
			},
		},
		{
			Name:        "muninn_merge_entity",
			Description: "Merge entity_a into entity_b (canonical). Sets entity_a state to merged, relinks all engrams in the vault from entity_a to entity_b, and updates entity_b mention count. Use dry_run=true to preview the operation without writing.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":    vaultProp,
					"entity_a": map[string]any{"type": "string", "description": "The entity name to be merged away (becomes state=merged)."},
					"entity_b": map[string]any{"type": "string", "description": "The canonical entity name to keep."},
					"dry_run":  map[string]any{"type": "boolean", "description": "When true, report what would happen without writing any data (default false)."},
				},
				"required": []string{"entity_a", "entity_b"},
			},
		},
		// Enrichment replay
		{
			Name:        "muninn_replay_enrichment",
			Description: "Re-run the enrichment pipeline for memories in a vault that are missing specific digest stages (entities, relationships, classification, summary). Use this to retroactively enrich memories that were stored before an LLM provider was configured, or to fill in specific pipeline stages that were skipped. Supports dry_run=true to preview what would be processed without writing. The response includes processed (successfully enriched), skipped (already enriched or nothing to enrich), failed (enrichment or persistence errors), and remaining (not reached before context deadline/cancellation) counts.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"stages": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string", "enum": []string{"entities", "relationships", "classification", "summary"}},
						"description": "Which enrichment stages to re-run. Defaults to all four: entities, relationships, classification, summary. Only memories missing these stages will be processed.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of memories to process in this call (default 50, max 200). Use multiple calls to process larger vaults incrementally.",
					},
					"dry_run": map[string]any{
						"type":        "boolean",
						"description": "When true, scan and count how many memories would be enriched without actually running enrichment. Use to gauge scope before committing (default false).",
					},
				},
				"required": []string{},
			},
		},
		// Provenance audit trail
		{
			Name:        "muninn_provenance",
			Description: "Returns the ordered audit trail for an engram — who wrote it, what changed, and why.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "Engram ID (ULID)."},
				},
				"required": []string{"id"},
			},
		},
		// Entity timeline
		{
			Name:        "muninn_entity_timeline",
			Description: "Return a chronological view of when an entity first appeared in memory and how it has evolved. Shows all engrams mentioning the entity, sorted by creation time (oldest first).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":       vaultProp,
					"entity_name": map[string]any{"type": "string", "description": "The entity name to look up (e.g. 'PostgreSQL', 'Alice')"},
					"limit":       map[string]any{"type": "integer", "description": "Max timeline entries to return (1-50, default 10)"},
				},
				"required": []string{"entity_name"},
			},
		},
		// SGD learning loop feedback
		{
			Name:        "muninn_feedback",
			Description: "Record explicit feedback on an engram. Use useful=false when a retrieved engram was not helpful. Updates the vault's learned scoring weights via SGD.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":     vaultProp,
					"engram_id": map[string]any{"type": "string", "description": "Engram ID that was retrieved"},
					"useful":    map[string]any{"type": "boolean", "description": "Whether the engram was helpful (default false = negative signal)"},
				},
				"required": []string{"engram_id"},
			},
		},
		// Entity aggregate view
		{
			Name:        "muninn_entity",
			Description: "Returns the full aggregate view for a named entity: metadata, engrams mentioning it, relationships, and co-occurring entities.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"name":  map[string]any{"type": "string", "description": "Entity name (case-insensitive)"},
					"limit": map[string]any{"type": "integer", "description": "Max engrams to include (default 20)"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "muninn_entities",
			Description: "Lists all known entities in a vault, sorted by mention count. Optionally filter by state (active, deprecated, merged, resolved).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"limit": map[string]any{"type": "integer", "description": "Max results (default 50)"},
					"state": map[string]any{"type": "string", "description": "Filter by state: active, deprecated, merged, resolved"},
				},
				"required": []string{},
			},
		},
		// Trust label
		{
			Name:        "muninn_trust",
			Description: "Set the trust level of an engram. Trust levels control how much confidence to place in a memory. Use 'verified' for human-confirmed facts, 'inferred' for AI-generated memories (default), 'external' for imported data, and 'untrusted' to flag unreliable memories. Untrusted memories can be excluded from recall by enabling ExcludeUntrusted in vault config.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "ULID of the engram to update",
					},
					"trust": map[string]any{
						"type":        "string",
						"enum":        []string{"verified", "inferred", "external", "untrusted"},
						"description": "Trust level to assign",
					},
					"vault": map[string]any{
						"type":        "string",
						"description": "Vault containing the engram (default: \"default\")",
					},
				},
				"required": []string{"id", "trust"},
			},
		},
		// THE PUSH: prospective memory — arm an intention on entity cues.
		{
			Name:        "muninn_intend",
			Description: "Arm a prospective intention: 'when <cue entity> comes up, surface <content>'. Stored as a goal memory and armed on one or more cue entities. It NEVER interrupts — it is delivered as a 'notices' field on a later muninn_recall/muninn_remember response whose results are actually about the cue entity (requires MUNINN_PROSPECTIVE=1 on the server). Cues must be specific: an entity mentioned by a large share of the vault is refused. valid_until only silences an expired intention; it never triggers delivery.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":   vaultProp,
					"content": map[string]any{"type": "string", "description": "What to surface when a cue entity becomes focal (delivered verbatim in the notice)."},
					"cues": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Entity names to arm on (1-8). The intention fires when one of these entities is focal in a later call's results. Prefer rare, specific entities; ubiquitous ones are rejected.",
					},
					"valid_until": map[string]any{"type": "string", "description": "ISO 8601 expiry. A BOUND, not a trigger: after this instant the intention is silenced, never fired. Optional."},
					"one_shot":    map[string]any{"type": "boolean", "description": "When true (default) the intention disarms after its first delivery. Set false for a recurring prompt (re-fires once per session while armed)."},
					"importance":  map[string]any{"type": "number", "description": "Priority in [0,1]; ranks this notice against others when more than 2 are eligible (default: goal-type derived 0.6)."},
				},
				"required": []string{"content", "cues"},
			},
		},
		// RFC #597: privileged workflow-vault creation (recursion-guarded in dispatchToolCall).
		{
			Name:        "muninn_create_workflow_vault",
			Description: "Create a shared working vault for an agentic workflow and mint a scoped, TTL'd capability token a worker agent can use to access it. The vault uses the `working` preset (default cognition + 7-day auto-evaporation) with multi_user enabled. Requires a full-mode mk_ key and the MUNINN_AGENT_VAULT_CREATE opt-in. The returned capability_secret is shown once — distribute it to worker agents out-of-band. Recursion-safe: a capability cannot call this tool.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "Vault name (optional; auto-generates wf-<8hex> if omitted). MUST start with 'wf-' and be 1-64 lowercase alphanum/hyphen/underscore. Names lacking the wf- prefix are rejected (prevents cross-vault clobber)."},
					"label":     map[string]any{"type": "string", "default": "agent-minted", "description": "Label stamped on the minted capability (for audit/listing)."},
					"ttl_hours": map[string]any{"type": "integer", "default": 168, "minimum": float64(1), "maximum": float64(168), "description": "Capability lifetime in hours (1-168; default 168 = 7d, matching the working preset retention). Sub-hour values floor to 1; values above 168 clamp to 168."},
				},
			},
		},
	}
}
