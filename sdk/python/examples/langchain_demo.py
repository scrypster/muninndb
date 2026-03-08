"""MuninnDB + LangChain integration demo.

Demonstrates MuninnDBMemory as a drop-in LangChain memory backend.

What this shows:
  - Writing memories via the MuninnDB SDK directly
  - Using MuninnDBMemory in a LangChain ConversationChain
  - How relevant past context surfaces automatically in later turns
    without any explicit retrieval query from the user

Prerequisites:
  1. MuninnDB running locally:
       muninn start
     or:
       docker compose up -d

  2. Install dependencies:
       pip install muninn-python[langchain] langchain langchain-anthropic

  3. For real LLM usage, set your API key:
       export ANTHROPIC_API_KEY=sk-ant-...

Run:
    python examples/langchain_demo.py
    python examples/langchain_demo.py --real-llm   # uses Claude Haiku
"""

from __future__ import annotations

import argparse
import asyncio
import sys

from muninn import MuninnClient
from muninn.langchain import MuninnDBMemory

MUNINN_URL = "http://localhost:8475"
VAULT = "default"


# ── Part 1: Seed some memories directly via the SDK ─────────────────────────

async def seed_memories() -> list[str]:
    """Write a set of project memories that an agent might have accumulated."""
    memories = [
        {
            "concept": "payment service double-charge incident",
            "content": (
                "In Q3 we had a double-charge incident in the payment service. "
                "Root cause: the retry logic on the idempotency layer was not "
                "respecting the 202 Accepted response correctly. Fixed by switching "
                "to idempotency keys with a 24-hour TTL."
            ),
            "tags": ["payments", "bug", "incident"],
        },
        {
            "concept": "database migration strategy",
            "content": (
                "We chose zero-downtime migrations using expand-contract pattern. "
                "All migrations must be backward-compatible for at least one release cycle. "
                "Rollback tested in staging before every production deploy."
            ),
            "tags": ["database", "migrations", "infra"],
        },
        {
            "concept": "authentication service architecture",
            "content": (
                "Auth uses short-lived JWTs (15 min) with refresh tokens stored in "
                "HttpOnly cookies. Sessions are server-side in Redis. OAuth2 flows "
                "handled by the auth-service, not individual microservices."
            ),
            "tags": ["auth", "security", "architecture"],
        },
        {
            "concept": "on-call rotation schedule",
            "content": (
                "On-call rotates weekly on Monday 9am. Primary and secondary engineers "
                "designated. Escalation SLA: 5 min for P0, 30 min for P1, next business "
                "day for P2. PagerDuty is the source of truth."
            ),
            "tags": ["ops", "on-call", "process"],
        },
        {
            "concept": "API rate limiting policy",
            "content": (
                "Public API: 100 req/min per token. Internal APIs: 10k req/min. "
                "Rate limits enforced at the gateway layer. Headers: X-RateLimit-Remaining, "
                "X-RateLimit-Reset. 429 response with Retry-After header."
            ),
            "tags": ["api", "rate-limiting", "policy"],
        },
    ]

    print("Seeding project memories into MuninnDB...")
    async with MuninnClient(MUNINN_URL) as client:
        ids = []
        for mem in memories:
            eid = await client.write(
                vault=VAULT,
                concept=mem["concept"],
                content=mem["content"],
                tags=mem["tags"],
                confidence=0.95,
            )
            ids.append(eid)
            print(f"  ✓ {mem['concept']}")

    print(f"\n{len(ids)} memories stored.\n")
    return ids


# ── Part 2: Demonstrate memory activation without a real LLM ────────────────

async def demo_activation_only():
    """Show how MuninnDBMemory surfaces relevant context for different queries.

    This runs without any LLM API key — it demonstrates the memory retrieval
    layer in isolation so you can verify it works before wiring up a real chain.
    """
    memory = MuninnDBMemory(base_url=MUNINN_URL, vault=VAULT, max_results=3)

    queries = [
        "We're seeing duplicate charges in production — what should I check?",
        "How do users authenticate with our API?",
        "Who do I call if the payment service goes down at 2am?",
        "What's our retry policy for database writes?",
    ]

    print("=" * 60)
    print("Memory Activation Demo (no LLM required)")
    print("=" * 60)
    print()

    for query in queries:
        print(f"Query: {query!r}")
        result = memory.load_memory_variables({"input": query})
        context = result.get("history", "")
        if context:
            # Print just the bullet points for readability.
            for line in context.splitlines():
                if line.startswith("-"):
                    print(f"  {line}")
        else:
            print("  (no relevant memories found)")
        print()


# ── Part 3: Full LangChain ConversationChain with a real LLM ────────────────

def demo_with_real_llm():
    """Run a multi-turn conversation using MuninnDB as the memory backend.

    Requires: pip install langchain langchain-anthropic
    And: export ANTHROPIC_API_KEY=sk-ant-...
    """
    try:
        from langchain.chains import ConversationChain
        from langchain.prompts import PromptTemplate
        from langchain_anthropic import ChatAnthropic
    except ImportError:
        print("For the real LLM demo, install: pip install langchain langchain-anthropic")
        sys.exit(1)

    memory = MuninnDBMemory(base_url=MUNINN_URL, vault=VAULT, max_results=5)

    # Custom prompt that shows the memory context to the LLM.
    prompt = PromptTemplate(
        input_variables=["history", "input"],
        template=(
            "You are a helpful engineering assistant with access to project memory.\n\n"
            "{history}\n\n"
            "Current question: {input}\n"
            "Assistant:"
        ),
    )

    chain = ConversationChain(
        llm=ChatAnthropic(model="claude-haiku-4-5-20251001"),
        memory=memory,
        prompt=prompt,
        verbose=True,
    )

    # These questions touch on memories seeded above.
    # Watch how Claude uses the surfaced context without being told to.
    turns = [
        "We're debugging a payment issue where some customers got charged twice. Any history on this?",
        "Who handles the auth layer — is it per-service or centralized?",
        "If something breaks tonight, what's the escalation process?",
    ]

    print("=" * 60)
    print("LangChain ConversationChain with MuninnDB Memory")
    print("=" * 60)
    print()

    for turn in turns:
        print(f"Human: {turn}")
        response = chain.predict(input=turn)
        print(f"AI: {response}")
        print()


# ── Entry point ──────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="MuninnDB + LangChain demo")
    parser.add_argument(
        "--real-llm",
        action="store_true",
        help="Use Claude Haiku instead of simulated responses (requires ANTHROPIC_API_KEY)",
    )
    parser.add_argument(
        "--skip-seed",
        action="store_true",
        help="Skip seeding memories (useful if you've already run the demo once)",
    )
    args = parser.parse_args()

    if not args.skip_seed:
        asyncio.run(seed_memories())

    if args.real_llm:
        demo_with_real_llm()
    else:
        asyncio.run(demo_activation_only())
        print()
        print("─" * 60)
        print("The queries above show which memories MuninnDB surfaces for")
        print("each input — this is exactly what gets injected into the LLM")
        print("prompt as context when you use a real chain.")
        print()
        print("To run with a real LLM:")
        print("  export ANTHROPIC_API_KEY=sk-ant-...")
        print("  python examples/langchain_demo.py --real-llm")


if __name__ == "__main__":
    main()
