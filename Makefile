ASSETS_DIR := internal/plugin/embed/assets
# ONNX Runtime versions and their pinned sha256 live in scripts/fetch-ort-lib.sh.
MODEL_REPO  := Xenova/bge-small-en-v1.5
HF_BASE     := https://huggingface.co/$(MODEL_REPO)/resolve/main

.PHONY: fetch-assets fetch-model fetch-ort-libs clean-assets web css build test bench test-integration \
        corpora check-filenames check-nul-bytes proto proto-check \
        _ort-darwin-arm64 _ort-darwin-amd64 _ort-linux-amd64 _ort-linux-arm64 _ort-windows-amd64

## fetch-assets: download the model, tokenizer, and all platform ORT libraries.
fetch-assets: fetch-model fetch-ort-libs

## fetch-model: download model_int8.onnx and tokenizer.json from HuggingFace.
fetch-model:
	@echo "==> Downloading bge-small-en-v1.5 INT8 model..."
	@mkdir -p $(ASSETS_DIR)
	@curl -fL --progress-bar \
		"$(HF_BASE)/onnx/model_int8.onnx" \
		-o "$(ASSETS_DIR)/model_int8.onnx"
	@echo "==> Downloading tokenizer.json..."
	@curl -fL --progress-bar \
		"https://huggingface.co/BAAI/bge-small-en-v1.5/resolve/main/tokenizer.json" \
		-o "$(ASSETS_DIR)/tokenizer.json"
	@echo "    model_int8.onnx: $$(du -sh $(ASSETS_DIR)/model_int8.onnx | cut -f1)"
	@echo "    tokenizer.json:  $$(du -sh $(ASSETS_DIR)/tokenizer.json  | cut -f1)"
	@echo "==> Model assets ready."

## fetch-ort-libs: download ORT native libraries for all supported platforms.
fetch-ort-libs:
	@echo "==> Fetching ONNX Runtime native libraries..."
	@mkdir -p $(ASSETS_DIR)
	@$(MAKE) -s _ort-darwin-arm64
	@$(MAKE) -s _ort-darwin-amd64
	@$(MAKE) -s _ort-linux-amd64
	@$(MAKE) -s _ort-linux-arm64
	@$(MAKE) -s _ort-windows-amd64
	@echo "==> ORT native libraries ready."

# Each target installs one library through scripts/fetch-ort-lib.sh, which
# checks the pinned sha256 and falls back to the mirror when upstream fails.
_ort-darwin-arm64:
	@ASSETS_DIR=$(ASSETS_DIR) scripts/fetch-ort-lib.sh darwin-arm64

_ort-darwin-amd64:
	@ASSETS_DIR=$(ASSETS_DIR) scripts/fetch-ort-lib.sh darwin-amd64

_ort-linux-amd64:
	@ASSETS_DIR=$(ASSETS_DIR) scripts/fetch-ort-lib.sh linux-amd64

_ort-linux-arm64:
	@ASSETS_DIR=$(ASSETS_DIR) scripts/fetch-ort-lib.sh linux-arm64

_ort-windows-amd64:
	@ASSETS_DIR=$(ASSETS_DIR) scripts/fetch-ort-lib.sh windows-amd64

## clean-assets: remove downloaded binary assets.
clean-assets:
	@echo "==> Removing downloaded assets..."
	@rm -f $(ASSETS_DIR)/model_int8.onnx
	@rm -f $(ASSETS_DIR)/tokenizer.json
	@rm -f $(ASSETS_DIR)/libonnxruntime_*.dylib
	@rm -f $(ASSETS_DIR)/libonnxruntime_*.so
	@rm -f $(ASSETS_DIR)/onnxruntime_*.dll
	@echo "==> Done."

## web: compile Tailwind CSS via Vite (requires Node.js + npm).
web:
	@cd web && npm ci --silent && npm run build --silent

## css: rebuild only the CSS bundle (faster than full web target; skips npm ci).
css:
	@cd web && npm run build --silent

## build: build the server binary (requires fetch-assets and web first).
build: web
	@go build -tags localassets -o muninndb-server ./cmd/muninn/...

## test: run unit tests across all packages.
test:
	@go test ./...

## proto: regenerate gRPC stubs from proto/muninn/v1/service.proto.
## Requires protoc, protoc-gen-go (v1.36.11), protoc-gen-go-grpc (v1.6.2) on PATH.
proto:
	@echo "==> Regenerating gRPC stubs from service.proto..."
	@protoc \
		--proto_path=proto \
		--go_out=. --go_opt=module=github.com/scrypster/muninndb \
		--go-grpc_out=. --go-grpc_opt=module=github.com/scrypster/muninndb \
		muninn/v1/service.proto
	@echo "==> Stubs regenerated. Review with: git diff proto/gen/"

## proto-check: fail if committed gRPC stubs differ from a fresh regeneration.
## Regenerates into a temp copy of proto/gen and diffs it against the tree — catches
## hand-edits. Never writes to the working tree, so the edit it detects is still there
## afterwards. Fails CLOSED on drift AND on a missing/broken protoc toolchain. Run
## before pushing.
proto-check:
	@echo "==> Verifying committed stubs match service.proto..."
	@tmp=$$(mktemp -d) || exit 1; \
	trap 'rm -rf "$$tmp"' EXIT; \
	mkdir -p "$$tmp/proto" && cp -r proto/gen "$$tmp/proto/gen" && \
		protoc \
			--proto_path=proto \
			--go_out="$$tmp" --go_opt=module=github.com/scrypster/muninndb \
			--go-grpc_out="$$tmp" --go-grpc_opt=module=github.com/scrypster/muninndb \
			muninn/v1/service.proto && \
		(diff -r proto/gen "$$tmp/proto/gen" >/dev/null && echo "==> Stubs up to date.") || \
		(echo "##[error]committed stubs differ from protoc output, or the protoc/protoc-gen-go/protoc-gen-go-grpc toolchain is missing or broken. Run 'make proto' and commit, or install the toolchain."; exit 1)

## bench: run E2E benchmark suite.
bench:
	go test -bench=BenchmarkE2E -benchmem -benchtime=3s ./internal/bench/...

## test-integration: run integration tests (requires no muninn already running on :8750).
## Builds the binary, exercises the full start/stop/restart lifecycle, then cleans up.
## If :8750 is busy the suite SKIPs visibly; MUNINN_REQUIRE_INTEGRATION=1 makes that a
## failure instead, which is what CI sets (#812).
test-integration:
	@go test -tags integration -v -timeout 120s ./cmd/muninn/...

## corpora: run the standing measurement corpora and write .artifacts/corpora.txt.
## The four harnesses in internal/engine/activation, normalised so two runs of an
## unchanged tree are byte-identical and a diff shows only moved numbers. They also run
## inside `go test ./...`; this target exists to produce the diffable artifact.
corpora:
	@bash scripts/run-corpora.sh

## check-filenames: fail if a source file's NAME silently excludes it from the build (#814).
check-filenames:
	@bash scripts/check-filename-build-constraints.sh

## check-nul-bytes: fail if a tracked source file contains a NUL byte, which git renders as
## a binary blob invisible to git diff and every diff-based review surface (#827).
check-nul-bytes:
	@bash scripts/check-nul-bytes.sh

# The eval-bible-* targets were removed here: they build ./cmd/eval-bible/... and run
# scripts/eval-bible-setup.sh, neither of which is in the public repo (cmd/eval*/ and
# scripts/eval-* are gitignored as internal development tooling). Advertising targets that
# cannot run from a fresh clone is worse than not listing them. If the retrieval-quality
# eval harness is published later, restore them alongside it.
#
# testdata/bible/ went with them (it held only a .gitkeep). An empty data directory whose
# only loader is gitignored reads as "there is a corpus here"; there was not one, and it
# was cited as if it were the standing corpora. The standing corpora are `make corpora`.
