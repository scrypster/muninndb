"""Basic write → activate demo for MuninnDB.

This example demonstrates the fundamental cognitive loop:
1. Write engrams (memories) to the database
2. Query the database with semantic activation
3. Retrieve ranked, contextual results

Run with: python examples/write_activate.py
"""

import asyncio

from muninn import MuninnClient


async def main():
    """Write and activate example."""
    async with MuninnClient("http://localhost:8476") as client:
        print("MuninnDB Write → Activate Demo\n")

        # Write a few engrams about neuroscience
        memories = [
            {
                "concept": "neural plasticity",
                "content": "The brain's ability to reorganize neural connections. Hebbian learning: neurons that fire together wire together. Critical for learning and memory formation.",
                "tags": ["neuroscience", "learning", "plasticity"],
            },
            {
                "concept": "memory consolidation",
                "content": "The process of stabilizing a memory trace after initial acquisition. Sleep plays a critical role in consolidating declarative and procedural memories.",
                "tags": ["memory", "neuroscience", "sleep"],
            },
            {
                "concept": "synaptic pruning",
                "content": "Elimination of unused synapses during development and learning. Strengthens important connections while removing noise.",
                "tags": ["neuroscience", "development", "learning"],
            },
            {
                "concept": "long-term potentiation",
                "content": "Sustained increase in synaptic effectiveness. Molecular mechanism involves NMDA receptors and calcium influx. Basis of associative learning.",
                "tags": ["neuroscience", "learning", "synapses"],
            },
            {
                "concept": "dendritic spines",
                "content": "Small protrusions on dendrites where synaptic connections occur. Their size and number correlate with learning and memory.",
                "tags": ["neuroscience", "anatomy", "synapses"],
            },
        ]

        print("Writing engrams...")
        ids = []
        for mem in memories:
            eid = await client.write(
                vault="default",
                concept=mem["concept"],
                content=mem["content"],
                tags=mem["tags"],
                confidence=0.95,
            )
            ids.append(eid)
            print(f"  ✓ {mem['concept']}: {eid}")

        print(f"\nWrote {len(ids)} engrams\n")

        # Activate memory with a semantic query
        print("Activating memory with query: 'how does learning work in the brain?'")
        result = await client.activate(
            vault="default",
            context=["how does learning work", "neural mechanisms", "memory formation"],
            max_results=10,
            threshold=0.0,
            brief_mode="extractive",
        )

        print(f"\nFound {result.total_found} relevant memories (query_id: {result.query_id})")
        print(f"Query latency: {result.latency_ms:.1f}ms\n")

        print("Activations (ranked by relevance):")
        for i, item in enumerate(result.activations, 1):
            print(
                f"  {i}. [{item.score:.3f}] {item.concept} (confidence: {item.confidence:.2f})"
            )
            print(f"     {item.content[:90]}...")

        # Show brief if available
        if result.brief:
            print(f"\nBrief (top {len(result.brief)} extractive sentences):")
            for i, sent in enumerate(result.brief, 1):
                print(f"  {i}. [{sent.score:.2f}] {sent.text}")

        # Get database stats
        print("\n" + "=" * 50)
        stats = await client.stats()
        print(f"\nDatabase statistics:")
        print(f"  Total engrams: {stats.engram_count}")
        print(f"  Total vaults: {stats.vault_count}")
        print(f"  Storage: {stats.storage_bytes:,} bytes")

        if stats.coherence:
            print(f"\nCoherence metrics:")
            for vault_name, coherence in stats.coherence.items():
                print(f"  Vault '{vault_name}':")
                print(f"    Score: {coherence.score:.3f}")
                print(f"    Orphan ratio: {coherence.orphan_ratio:.3f}")
                print(f"    Duplication pressure: {coherence.duplication_pressure:.3f}")


if __name__ == "__main__":
    asyncio.run(main())
