# MuninnDB — Documentation Index

An intent-organized reading guide. Start with what you want to understand.

---

## If you want to understand what MuninnDB is

1. **[How Memory Works](how-memory-works.md)** — Explains what an engram is and how memories are stored, scored, and retrieved.
2. **[vs. Other Databases](vs-other-databases.md)** — Positions MuninnDB against vector stores, graph databases, and key-value stores.

---

## If you want to understand how memory is structured

1. **[Engram](engram.md)** — The core data structure: fields, lifecycle states, and key-space layout.
2. **[Cognitive Primitives](cognitive-primitives.md)** — Hebbian learning, temporal decay, activation spread, and Bayesian confidence.

---

## If you want to understand how retrieval works

1. **[Retrieval Design](retrieval-design.md)** — The 6-phase ACTIVATE pipeline: how recall queries are processed.
2. **[Architecture](architecture.md)** — System components, data flow, and the Pebble storage layer.

---

## If you want to use MuninnDB

1. **[Quickstart](quickstart.md)** — Getting MuninnDB running and making your first memory writes.
2. **[Feature Reference](feature-reference.md)** — Complete reference for all 35 MCP tools and their parameters.

---

## If you want to organize memory

1. **[Hierarchical Memory](hierarchical-memory.md)** — Tree-structured memory for outlines, plans, and task hierarchies.
2. **[Entity Graph](entity-graph.md)** — Named entity extraction, relationships, and the cross-vault entity index.

---

## If you want to understand how storage works

1. **[Key-Space Schema](key-space-schema.md)** — All key-space prefix bytes (0x01–0x24) and their storage layouts.
2. **[Durability Guarantees](durability-guarantees.md)** — Sync vs NoSync write paths and the WAL group-commit contract.

---

## If you want to deploy MuninnDB

1. **[Self-Hosting](self-hosting.md)** — Deployment options, environment variables, and data directory setup.
2. **[Cluster Operations](cluster-operations.md)** — Multi-node clustering, replication, and leader election.
3. **[Claude.com / ChatGPT via Traefik](integrations/traefik-claude-chatgpt.md)** — Connect a cloud-hosted MuninnDB to Claude.com Connectors or ChatGPT's MCP support.

---

## If you want to understand how auth works

1. **[Auth](auth.md)** — Vault-scoped API keys and authentication model.

---

## If you want to understand how plugins work

1. **[Plugins](plugins.md)** — Embedding model plugins and the plugin interface.
2. **[Semantic Triggers](semantic-triggers.md)** — Trigger rules, conditions, and the trigger worker pipeline.

---

## If you want a complete feature overview

1. **[Capabilities](capabilities.md)** — Comprehensive technical capability statement with code references.
2. **[Feature Reference](feature-reference.md)** — Complete reference for all 35 MCP tools and their parameters.
