## Summary

<!-- What does this PR do? Why? -->

## Changes

<!-- Bullet points describing what changed -->

## Release Checklist

- [ ] ~~`CHANGELOG.md`~~ — maintainers update this at release time, no action needed
- [ ] OpenAPI spec (`internal/transport/rest/openapi.yaml`) updated if API routes changed
- [ ] SDK types updated if request/response schemas changed (Python, Node, PHP)
- [ ] `docs/` updated if user-facing behavior changed
- [ ] `go build ./...` clean
- [ ] `go vet ./...` clean
- [ ] `go test ./...` passes
