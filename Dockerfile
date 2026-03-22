# Stage 1: Fetch assets and build
# go:embed directives require the ORT native library and ONNX model to exist at compile time.
FROM golang:1.24-bookworm AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src

ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl make ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node.js for Tailwind/Vite CSS build
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

COPY . .

# Fetch model + platform-specific ORT native library.
# TARGETARCH is injected by Docker Buildx (amd64 or arm64).
# - model_int8.onnx + tokenizer.json: required by local_assets_common.go go:embed
# - libonnxruntime_linux_{arch}.so:   required by local_assets_linux_{arch}.go go:embed
RUN make fetch-model _ort-linux-${TARGETARCH}

# Build web assets (Tailwind CSS via Vite)
RUN cd web && npm ci --ignore-scripts && npm run build

# Build the server binary.
# CGO_ENABLED=0 would break the local ONNX embedder (dlopen at runtime).
# The binary links against glibc — debian-slim provides it in the runtime stage.
RUN go build -tags localassets -ldflags="-s -w" -o /muninndb-server ./cmd/muninn/...

# Stage 2: Minimal runtime image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /muninndb-server /usr/local/bin/muninndb-server

# Persistent data volume — Pebble DB, WAL, and auth secrets live here.
VOLUME ["/data"]

# MBP protocol  8474
# REST API       8475
# Web UI         8476
# gRPC           8477
# MCP / AI tools 8750
EXPOSE 8474 8475 8476 8477 8750

# Bundled all-MiniLM-L6-v2 embedder is active by default.
# Set MUNINN_LOCAL_EMBED=0 to disable and use MUNINN_OPENAI_KEY or MUNINN_OLLAMA_URL instead.

ENTRYPOINT ["muninndb-server"]
CMD ["--daemon", "--data", "/data"]
