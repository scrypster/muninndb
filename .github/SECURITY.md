# Security Policy

MuninnDB is **alpha** software maintained by a very small team. This policy is written to
be honest about that rather than to promise more than we can deliver.

## Reporting a vulnerability

**Please report privately, not in a public issue.**

Use GitHub's private vulnerability reporting:
[**Report a vulnerability**](https://github.com/scrypster/muninndb/security/advisories/new).
It creates a private advisory only you and the maintainers can see, and it handles
coordinated disclosure and CVE requests if we get that far.

If you can, include:

- The version or commit you tested (`muninn version`)
- Which surface is affected — MCP, REST, gRPC, MBP, the web console, the CLI, or an SDK
- What an attacker gains, and what access they need to start
- The smallest reproduction you can manage

We will acknowledge reports and work through them on a **best-effort** basis. We are not
promising a response time we cannot honor. If something is being actively exploited, say so
in the report and we will treat it accordingly.

Please give us a reasonable chance to ship a fix before disclosing publicly. We would rather
credit you in the advisory than read about it first somewhere else — tell us how you want to
be credited, or if you would rather not be.

## Scope

**Supported version: the latest release.** Alpha means we do not backport fixes to older
tags. If you are on an older version, the first step is upgrading.

In scope — anything that lets someone read, write, or destroy memory they should not reach:

- Authentication and authorization on any transport (`mk_` API keys, `cap_` capability
  tokens, static tokens, admin sessions)
- Cross-vault access, or a vault-pinned credential acting outside its vault
- Privilege escalation, including a capability token minting further credentials
- Data corruption or loss (storage, WAL, HNSW index, replication)
- Remote code execution, SSRF, or path traversal
- Secrets leaking through logs, errors, exports, or backups

Out of scope:

- **The default `root` / `password` admin credential.** This is a deliberate onboarding
  choice for a first-run, loopback-bound install, documented in the README. Change it in any
  deployment that is reachable from a network.
- Anything that requires an attacker to already have filesystem or OS-level access to the
  host — MuninnDB does not defend against a compromised machine.
- Missing hardening headers or TLS configuration on a deployment the operator chose to run
  without TLS. See [`docs/tls.md`](../docs/tls.md) and
  [`docs/self-hosting.md`](../docs/self-hosting.md).
- Denial of service through sheer volume against an instance you control.
- Findings from automated scanners with no demonstrated impact.

## Known weaknesses

MuninnDB is alpha and has rough edges we already know about. Some are tracked as public
issues; `docs/internals/invariants.md` documents the properties that must hold, and
`docs/internals/drift-and-obligations.md` lists surfaces known to be imperfect. If you find
something already tracked, a comment on that issue is more useful than a new report — but if
you think it is more severe than we rated it, tell us privately. We would rather re-rate our
own severity than defend it.
