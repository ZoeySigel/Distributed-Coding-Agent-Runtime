ARG BASE_REGISTRY=docker.io/library
FROM ${BASE_REGISTRY}/golang:1.24.13-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -o /out/dcar ./cmd/dcar && \
    CGO_ENABLED=0 go build -trimpath -o /out/dcar-workspace ./cmd/workspace

FROM ${BASE_REGISTRY}/debian:bookworm-slim AS services
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
WORKDIR /app
COPY config/ /app/config/
USER 1000:1000
CMD ["server"]

FROM ${BASE_REGISTRY}/node:22.14.0-bookworm-slim AS workspace
ARG CODEX_VERSION=0.114.0
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates python3 python3-pip python3-venv ripgrep && rm -rf /var/lib/apt/lists/* && \
    npm install -g @openai/codex@${CODEX_VERSION} && \
    python3 -m venv /opt/venv && /opt/venv/bin/pip install --no-cache-dir pytest==8.3.5 && \
    mkdir -p /workspace /baseline /home/agent /run/dcar && chown -R 1000:1000 /workspace /baseline /home/agent /run/dcar
COPY --from=build /usr/local/go /usr/local/go
COPY --from=build /out/dcar-workspace /usr/local/bin/dcar-workspace
ENV PATH="/opt/venv/bin:/usr/local/go/bin:${PATH}" HOME=/home/agent GOTOOLCHAIN=local PIP_TARGET=/home/agent/python PYTHONPATH=/home/agent/python
USER 1000:1000
WORKDIR /workspace
CMD ["/usr/local/bin/dcar-workspace", "supervise"]
