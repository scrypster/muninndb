ASSETS_DIR := internal/plugin/embed/assets
ORT_VERSION := 1.24.2
# ORT dropped Intel Mac support after v1.23.2; pin darwin/amd64 to the last working version.
ORT_VERSION_DARWIN_AMD64 := 1.23.2
MODEL_REPO  := Xenova/bge-small-en-v1.5
HF_BASE     := https://huggingface.co/$(MODEL_REPO)/resolve/main
ORT_BASE    := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)

.PHONY: fetch-assets fetch-model fetch-ort-libs clean-assets web build test bench test-integration \
        eval-bible-setup eval-bible eval-bible-full eval-bible-quick eval-bible-export eval-bible-fast \
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
	@echo "==> Downloading ORT $(ORT_VERSION) native libraries..."
	@mkdir -p $(ASSETS_DIR)
	@$(MAKE) -s _ort-darwin-arm64
	@$(MAKE) -s _ort-darwin-amd64
	@$(MAKE) -s _ort-linux-amd64
	@$(MAKE) -s _ort-linux-arm64
	@$(MAKE) -s _ort-windows-amd64
	@echo "==> ORT native libraries ready."

_ort-darwin-arm64:
	@echo "    Fetching darwin/arm64..."
	@curl -fL --progress-bar \
		"$(ORT_BASE)/onnxruntime-osx-arm64-$(ORT_VERSION).tgz" \
		-o "/tmp/ort-osx-arm64.tgz"
	@tar -xzf /tmp/ort-osx-arm64.tgz -C /tmp onnxruntime-osx-arm64-$(ORT_VERSION)/lib/libonnxruntime.dylib 2>/dev/null || \
		tar -xzf /tmp/ort-osx-arm64.tgz -C /tmp --strip-components=2 --wildcards '*/lib/libonnxruntime.dylib'
	@cp /tmp/onnxruntime-osx-arm64-$(ORT_VERSION)/lib/libonnxruntime.dylib $(ASSETS_DIR)/libonnxruntime_darwin_arm64.dylib 2>/dev/null || \
		cp /tmp/libonnxruntime.dylib $(ASSETS_DIR)/libonnxruntime_darwin_arm64.dylib
	@echo "    darwin/arm64: $$(du -sh $(ASSETS_DIR)/libonnxruntime_darwin_arm64.dylib | cut -f1)"

_ort-darwin-amd64:
	@echo "    Fetching darwin/amd64 (ORT $(ORT_VERSION_DARWIN_AMD64) — last release with Intel Mac support)..."
	@curl -fL --progress-bar \
		"https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION_DARWIN_AMD64)/onnxruntime-osx-x86_64-$(ORT_VERSION_DARWIN_AMD64).tgz" \
		-o "/tmp/ort-osx-amd64.tgz"
	@rm -rf /tmp/ort-osx-amd64-extract && mkdir -p /tmp/ort-osx-amd64-extract
	@tar -xzf /tmp/ort-osx-amd64.tgz -C /tmp/ort-osx-amd64-extract/
	@find /tmp/ort-osx-amd64-extract/ -name 'libonnxruntime*.dylib' | head -1 | xargs -I{} cp {} $(ASSETS_DIR)/libonnxruntime_darwin_amd64.dylib
	@echo "    darwin/amd64: $$(du -sh $(ASSETS_DIR)/libonnxruntime_darwin_amd64.dylib | cut -f1)"

_ort-linux-amd64:
	@echo "    Fetching linux/amd64..."
	@curl -fL --progress-bar \
		"$(ORT_BASE)/onnxruntime-linux-x64-$(ORT_VERSION).tgz" \
		-o "/tmp/ort-linux-amd64.tgz"
	@tar -xzf /tmp/ort-linux-amd64.tgz -C /tmp onnxruntime-linux-x64-$(ORT_VERSION)/lib/libonnxruntime.so.$(ORT_VERSION) 2>/dev/null || \
		tar -xzf /tmp/ort-linux-amd64.tgz -C /tmp --strip-components=2 --wildcards '*/lib/libonnxruntime.so.*'
	@cp /tmp/onnxruntime-linux-x64-$(ORT_VERSION)/lib/libonnxruntime.so.$(ORT_VERSION) $(ASSETS_DIR)/libonnxruntime_linux_amd64.so 2>/dev/null || \
		find /tmp -name 'libonnxruntime.so.*' | head -1 | xargs -I{} cp {} $(ASSETS_DIR)/libonnxruntime_linux_amd64.so
	@echo "    linux/amd64: $$(du -sh $(ASSETS_DIR)/libonnxruntime_linux_amd64.so | cut -f1)"

_ort-linux-arm64:
	@echo "    Fetching linux/arm64..."
	@curl -fL --progress-bar \
		"$(ORT_BASE)/onnxruntime-linux-aarch64-$(ORT_VERSION).tgz" \
		-o "/tmp/ort-linux-arm64.tgz"
	@tar -xzf /tmp/ort-linux-arm64.tgz -C /tmp onnxruntime-linux-aarch64-$(ORT_VERSION)/lib/libonnxruntime.so.$(ORT_VERSION) 2>/dev/null || \
		tar -xzf /tmp/ort-linux-arm64.tgz -C /tmp --strip-components=2 --wildcards '*/lib/libonnxruntime.so.*'
	@cp /tmp/onnxruntime-linux-aarch64-$(ORT_VERSION)/lib/libonnxruntime.so.$(ORT_VERSION) $(ASSETS_DIR)/libonnxruntime_linux_arm64.so 2>/dev/null || \
		find /tmp -name 'libonnxruntime.so.*' | head -1 | xargs -I{} cp {} $(ASSETS_DIR)/libonnxruntime_linux_arm64.so
	@echo "    linux/arm64: $$(du -sh $(ASSETS_DIR)/libonnxruntime_linux_arm64.so | cut -f1)"

_ort-windows-amd64:
	@echo "    Fetching windows/amd64..."
	@curl -fL --progress-bar \
		"$(ORT_BASE)/onnxruntime-win-x64-$(ORT_VERSION).zip" \
		-o "/tmp/ort-win-x64.zip"
	@rm -rf /tmp/ort-win-x64-extract && mkdir -p /tmp/ort-win-x64-extract
	@unzip -q /tmp/ort-win-x64.zip -d /tmp/ort-win-x64-extract
	@find /tmp/ort-win-x64-extract -name 'onnxruntime.dll' | head -1 | xargs -I{} cp {} $(ASSETS_DIR)/onnxruntime_windows_amd64.dll
	@echo "    windows/amd64: $$(du -sh $(ASSETS_DIR)/onnxruntime_windows_amd64.dll | cut -f1)"

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

## build: build the server binary (requires fetch-assets and web first).
build: web
	@go build -o muninndb-server ./cmd/muninn/...

## test: run unit tests across all packages.
test:
	@go test ./...

## bench: run E2E benchmark suite.
bench:
	go test -bench=BenchmarkE2E -benchmem -benchtime=3s ./internal/bench/...

## test-integration: run integration tests (requires no muninn already running on :8750).
## Builds the binary, exercises the full start/stop/restart lifecycle, then cleans up.
test-integration:
	@go test -tags integration -v -timeout 120s ./cmd/muninn/...

## eval-bible-setup: download KJV and cross-reference data files.
eval-bible-setup:
	@bash scripts/eval-bible-setup.sh

## eval-bible: build the eval binary (NT-only corpus, 100 seeds).
eval-bible:
	@go build -o eval-bible ./cmd/eval-bible/...

## eval-bible-quick: run NT-only eval with 20 seeds (fast smoke test).
eval-bible-quick: eval-bible
	@./eval-bible -seeds 20 -min-xrefs 3

## eval-bible-export: run NT-only eval, export vault snapshot for fast re-runs.
eval-bible-export: eval-bible
	@./eval-bible -seeds 100 -export-to testdata/bible/bible-nt.muninn

## eval-bible-fast: run NT-only eval using pre-exported vault snapshot (skips 12-min load).
eval-bible-fast: eval-bible
	@./eval-bible -seeds 100 -import-from testdata/bible/bible-nt.muninn

## eval-bible-full: run full Bible eval (OT + NT corpus, 100 seeds).
eval-bible-full: eval-bible
	@./eval-bible -full -seeds 100
