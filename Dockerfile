# Multi-stage build for the audiosilo-sidecars daemon + embedded web UI.
#
# Stage 1 builds the SPA (Node 24). Stage 2 embeds it into a CGO-free Go binary
# with `-tags embedui`. Stage 3 is a slim Debian runtime that ships ffmpeg +
# ffprobe (the M2 audio stages need them). Because they are on $PATH, the daemon's
# toolfetch resolves them locally and NEVER auto-downloads inside the container.
#
# NOTE: whisper/agent CLIs (ASR + the agent runner, M3+) are still NOT bundled -
# later milestones add those tool dependencies and a heavier GPU runtime variant
# (see the plan's Dockerfile.cuda).

# --- Stage 1: build the web SPA -------------------------------------------------
FROM node:24-slim AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Stage 2: build the Go binary with the UI embedded --------------------------
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Place the built SPA where `-tags embedui` embeds it from.
COPY --from=web /web/dist ./internal/web/dist
ARG VERSION=0.0.0-docker
RUN CGO_ENABLED=0 go build -tags embedui \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/audiosilo-sidecars ./cmd/audiosilo-sidecars

# --- Stage 3: slim runtime with ffmpeg/ffprobe ----------------------------------
# debian-slim (not distroless/static) so the daemon can exec real ffmpeg/ffprobe:
# a downloaded static build is fragile on a static-only base, and bundling the
# tools means toolfetch never has to reach the network at runtime. Kept lean -
# --no-install-recommends and the apt lists are removed - and non-root.
FROM debian:stable-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 65532 --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot
COPY --from=build /out/audiosilo-sidecars /usr/local/bin/audiosilo-sidecars
USER nonroot
EXPOSE 8090
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/audiosilo-sidecars", "serve", "--data", "/data", "--listen", "0.0.0.0:8090"]
