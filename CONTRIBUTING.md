# Contributing to MuninnDB

**Let's build the memory layer for agents together.**

MuninnDB exists for the AI agent community — indie hackers wiring up Claude, researchers experimenting with cognitive architectures, or anyone who wants better long-term memory in their tools. Your input makes it better.

The project is under the [Business Source License 1.1](LICENSE) and has a provisional patent on the core cognitive primitives. Those exist to protect the project so we can keep developing it full-time and prevent big-tech forks from closing the open vision. **Normal use, modifications, and contributions are welcome and encouraged.** All contributions are licensed under the same BSL 1.1 (see [CLA](CLA.md)).

---

## How to contribute

1. **Bug reports or ideas** — Open an issue. Even a one-line "this broke my agent workflow" helps.
2. **Code changes** — Fork, make the change, open a PR.
   - Small fixes (typos, docs, tests) → we'll merge quickly.
   - New features (extra embedders, SDK improvements, UI) → open an issue first so we can align on the cognitive model.
3. **Documentation** — Improving examples, adding recipes, or translating → always valuable.
4. **Testing** — Run MuninnDB on your workloads and share results. Real-world numbers help.

**Development setup:** Clone the repo, then build and run from source:

```bash
git clone https://github.com/scrypster/muninndb
cd muninndb
go run ./cmd/muninn/... start
```

See the [README](README.md) for full install options and configuration.

---

## Branch model (Git Flow)

We use a **develop → main** flow:

| Branch    | Purpose |
|----------|---------|
| `main`   | Production-ready code. Only updated when we release. |
| `develop`| Integration branch. All feature/fix work is merged here first. |
| `feature/*`, `fix/*`, `bug/*` | Short-lived branches for your work. Merge into `develop` via PR. |

**Flow:**

1. **Start from `develop`:**  
   `git checkout develop && git pull origin develop`  
   Create your branch: `feature/my-thing`, `fix/issue-123`, or `bug/crash-on-x`.

2. **Work and push:**  
   Commit, push your branch, open a **pull request into `develop`**.

3. **CI must pass:**  
   The PR will run our CI (build, tests, Windows smoke, Python SDK). Fix any failures.

4. **Merge into `develop`:**  
   Once the PR is approved (if required) and CI is green, merge. Do **not** push directly to `develop` or `main`.

5. **Releases:**  
   When we're ready to release, we merge `develop` into `main` (via PR) and push a version tag (e.g. `v0.2.4`). That triggers builds, PyPI publish, PHP Packagist sync, and GitHub Release.

## Summary

- **feature/fix/bug** → open PR → **develop**
- **develop** → open PR → **main** (when releasing)
- **Tag on main** → release artifacts and package publishes

---

## Legal

By contributing, you agree that your contributions will be licensed under the [Business Source License 1.1](LICENSE) and that you have read our [Contributor License Agreement](CLA.md).
