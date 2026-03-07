# muninndb

`muninndb` is a convenience alias for [`muninn-python`](https://pypi.org/p/muninn-python), the official Python SDK for [MuninnDB](https://muninndb.com).

## Install

```bash
pip install muninndb                  # core SDK
pip install muninndb[langchain]       # + LangChain memory integration
```

Both are equivalent to installing `muninn-python` directly. Use whichever name feels more natural.

## Quick Start

```python
import asyncio
from muninn import MuninnClient

async def main():
    async with MuninnClient("http://127.0.0.1:8475") as client:
        eid = await client.write(
            vault="default",
            concept="neural plasticity",
            content="The brain's ability to reorganize neural connections.",
        )
        results = await client.activate(vault="default", context=["how does learning work?"])
        for item in results.activations:
            print(f"[{item.score:.2f}] {item.concept}")

asyncio.run(main())
```

## LangChain Integration

```python
from muninn.langchain import MuninnDBMemory
from langchain.chains import ConversationChain
from langchain_anthropic import ChatAnthropic

memory = MuninnDBMemory(vault="my-agent")
chain = ConversationChain(
    llm=ChatAnthropic(model="claude-haiku-4-5-20251001"),
    memory=memory,
)
chain.predict(input="What did we discuss about the payment service?")
```

Full documentation: [sdk/python/README.md](https://github.com/scrypster/muninndb/blob/main/sdk/python/README.md)
