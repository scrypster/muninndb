package mcp

func allToolDefinitions() []ToolDefinition {
	vaultProp := map[string]any{
		"type":        "string",
		"description": "Vault name to scope the operation (default: 'default'). Optional when connected via a vault-pinned MCP session.",
	}
	return []ToolDefinition{
		{
			Name:        "muninn_remember",
			Description: "Store a new piece of information (engram) in long-term memory. IMPORTANT: Keep each memory atomic — one concept, decision, or fact per memory. If a conversation covers multiple topics, use muninn_remember_batch to store them as separate memories. Atomic memories produce sharper recall, better associations, and more accurate contradiction detection. TIP: Provide ‘entities’ and ‘entity_relationships’ whenever you can identify them — this builds the knowledge graph immediately without requiring background enrichment.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":      vaultProp,
					"content":    map[string]any{"type": "string", "description": "The information to remember."},
					"concept":    map[string]any{"type": "string", "description": "Short label for this memory."},
					"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional topic tags."},
					"confidence": map[string]any{"type": "number", "description": "Confidence score 0.0-1.0 (default 1.0)."},
					"created_at": map[string]any{"type": "string", "description": "ISO 8601 timestamp for when this memory was created. Defaults to now. Use to seed memories at past or future times."},
				"type": map[string]any{"type": "string", "description": "Memory type — either a built-in name (fact, decision, observation, preference, issue, task, procedure, event, goal, constraint, identity, reference) or a free-form label (e.g. 'architectural_decision', 'coding_pattern'). Built-in names set the enum; free-form labels are stored as type_label with enum defaulting to 'fact'."},
				"type_label": map[string]any{"type": "string", "description": "Explicit free-form type label (e.g. 'architectural_decision'). Overrides the label inferred from 'type'."},
				"summary": map[string]any{"type": "string", "description": "One-line summary of what this memory captures. Providing this skips background summarization."},
				"entities": map[string]any{
					"type":        "array",
					"description": "Entities mentioned in this memory. Providing these skips background entity extraction.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name": map[string]any{"type": "string", "description": "Entity name (e.g. 'PostgreSQL', 'Auth Service')."},
							"type": map[string]any{"type": "string", "description": "Entity type (e.g. 'database', 'service', 'person', 'project')."},
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
							"content":    map[string]any{"type": "string", "description": "The information to remember."},
							"concept":    map[string]any{"type": "string", "description": "Short label for this memory."},
							"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional topic tags."},
							"confidence": map[string]any{"type": "number", "description": "Confidence score 0.0-1.0 (default 1.0)."},
							"created_at": map[string]any{"type": "string", "description": "ISO 8601 timestamp. Defaults to now."},
							"type":       map[string]any{"type": "string", "description": "Memory type — built-in name or free-form label."},
							"type_label": map[string]any{"type": "string", "description": "Explicit free-form type label."},
							"summary":    map[string]any{"type": "string", "description": "One-line summary. Skips background summarization."},
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
					"threshold": map[string]any{"type": "number", "description": "Minimum relevance score 0.0-1.0 (default 0.5)."},
					"limit":     map[string]any{"type": "integer", "description": "Max results to return (default 10)."},
					"profile": map[string]any{
						"type":        "string",
						"description": "Traversal profile for BFS graph traversal. Leave unset for automatic inference from your context phrases.\n• default       — balanced retrieval across all edge types; contradiction edges dampened (0.3×)\n• causal        — follow cause/effect/dependency chains (Causes, DependsOn, Blocks, PrecededBy, FollowedBy)\n• confirmatory  — find supporting evidence; contradiction edges excluded (Supports, Implements, Refines, References)\n• adversarial   — surface conflicts and contradictions (Contradicts, Supersedes, Blocks; Contradicts boosted 1.5×)\n• structural    — follow project/person/hierarchy edges (IsPartOf, BelongsToProject, CreatedByPerson)\n\nWhen to specify explicitly:\n  Use 'causal' when asking why something happened or what something depends on.\n  Use 'adversarial' when auditing for inconsistencies or contradictions.\n  Use 'confirmatory' when looking for supporting evidence for a claim.\n  Use 'structural' when navigating project or organizational structure.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"semantic", "recent", "balanced", "deep"},
						"description": "Recall mode preset.\n• semantic  — high-precision vector search (threshold=0.3)\n• recent    — recency-biased, 1 hop (threshold=0.2)\n• balanced  — engine defaults (no override)\n• deep      — exhaustive graph traversal, 4 hops (threshold=0.1)",
					},
					"since": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp (e.g. 2026-01-15T00:00:00Z). Only return memories created after this time.",
					},
					"before": map[string]any{
						"type":        "string",
						"description": "ISO 8601 timestamp (e.g. 2026-01-20T00:00:00Z). Only return memories created before this time.",
					},
				},
				"required": []string{"context"},
			},
		},
		{
			Name:        "muninn_read",
			Description: "Fetch a single memory by its ID.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "Memory ID (ULID)."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "muninn_forget",
			Description: "Soft-delete a memory. It remains recoverable but is excluded from recall.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"id":    map[string]any{"type": "string", "description": "Memory ID to forget."},
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
			Description: "Update a memory with new information. Creates a new version and archives the old one.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault":       vaultProp,
					"id":          map[string]any{"type": "string", "description": "ID of the memory to evolve."},
					"new_content": map[string]any{"type": "string", "description": "Updated information."},
					"reason":      map[string]any{"type": "string", "description": "Why this memory is being updated."},
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
			Description: "Get a summary of recent memory activity since a timestamp.",
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
			Description: "Surface what was being worked on at the end of the last session. Returns the most recently accessed active memories, sorted by recency. Call this at session start to orient yourself before any user queries.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"vault": vaultProp,
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max memories to return (default 10, max 50).",
					},
				},
				"required": []string{},
			},
		},
		// Entity reverse index tool
		{
			Name:        "muninn_find_by_entity",
			Description: "Return all memories that mention a given named entity. Uses the entity reverse index for fast O(matches) lookup.",
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
			Description: "Set the lifecycle state of a named entity (active, deprecated, merged, resolved). For state=merged, provide merged_into with the canonical entity name.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity_name": map[string]any{"type": "string", "description": "The entity name to update"},
					"state":       map[string]any{"type": "string", "description": "New state: active, deprecated, merged, or resolved"},
					"merged_into": map[string]any{"type": "string", "description": "Canonical entity name (required when state=merged)"},
					"vault":       vaultProp,
				},
				"required": []string{"entity_name", "state"},
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
							"concept":  map[string]any{"type": "string", "description": "Short label for this node."},
							"content":  map[string]any{"type": "string", "description": "Content for this node."},
							"type":     map[string]any{"type": "string", "description": "Memory type (goal, task, etc.)."},
							"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"children": map[string]any{"type": "array", "description": "Child nodes (same schema, recursive)."},
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
					"vault":  vaultProp,
					"format": map[string]any{"type": "string", "enum": []string{"json-ld", "graphml"}, "description": "Output format: 'json-ld' (default) or 'graphml'."},
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
			Description: "Re-run the enrichment pipeline for memories in a vault that are missing specific digest stages (entities, relationships, classification, summary). Use this to retroactively enrich memories that were stored before an LLM provider was configured, or to fill in specific pipeline stages that were skipped. Supports dry_run=true to preview what would be processed without writing.",
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
	}
}


