# Multi-stage build for the audiosilo-sidecars daemon + embedded web UI.
#
# Stage 1 builds the SPA (Node 24). Stage 2 embeds it into a CGO-free Go binary
# with `-tags embedui`. There are then TWO runtime targets sharing those stages:
#   - `runtime`      (default, last stage) - a slim Debian image (CPU ASR).
#   - `runtime-cuda`                       - an NVIDIA CUDA image (GPU ASR).
# `docker build .` builds the CPU image (the last stage); `docker build
# --target runtime-cuda .` builds the GPU one. The image workflow
# (.github/workflows/image.yml) builds both from this one file via `--target`,
# so the shared web/go build stages are defined once (no second Dockerfile to
# keep in sync).
#
# Both runtime images ship ffmpeg + ffprobe. Because they are on $PATH, the
# daemon's toolfetch resolves them locally and NEVER auto-downloads inside the
# container. The whisper.cpp CLI and ASR models are NOT bundled - they are
# toolfetched at runtime into <data>/tools (see whisper-binaries.yml, which ships
# them on its own cadence).

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

# --- Stage 3a: CUDA runtime (GPU ASR) -------------------------------------------
# GPU variant. This is the same daemon; only the runtime base differs.
#
# Runtime model (why this stage exists and what it does / does not ship):
#   - The CUDA whisper-cli is NOT baked into this image or any release. It ships
#     separately via .github/workflows/whisper-binaries.yml and the daemon's
#     toolfetch auto-downloads it AT RUNTIME into <data>/tools (device-detected,
#     sha256-verified). Keep it that way - the image stays lean and the whisper
#     build cadence stays decoupled from the image build.
#   - That CUDA whisper asset BUNDLES the CUDA runtime libs (cudart/cublas/
#     cublasLt) FLAT beside the binary with an $ORIGIN RPATH, so this image does
#     NOT need a CUDA toolkit installed. The only GPU dependency from outside the
#     container is libcuda.so.1 (the NVIDIA driver stub), which the
#     nvidia-container-toolkit injects at runtime when the container is started
#     with `--gpus all`.
#   - The daemon detects CUDA at runtime by running nvidia-smi, which the
#     nvidia-container-toolkit injects when NVIDIA_DRIVER_CAPABILITIES includes
#     `utility` (the nvidia/cuda base already sets `compute,utility`).
#
# nvidia/cuda:*-runtime is the "guaranteed path" for GPU users: ubuntu22.04 glibc
# matches the whisper CUDA build, and the base already sets the NVIDIA_* env vars
# so `--gpus all` injects the driver + nvidia-smi. It is intentionally the
# `runtime` (not `devel`) base - the whisper asset bundles its own cudart/cublas,
# so no CUDA toolkit is needed here.
#
# Run it with the NVIDIA container runtime, e.g.:
#   docker run --gpus all -p 8090:8090 -v sidecars-data:/data \
#     ghcr.io/kodestar/audiosilo-sidecars:latest-cuda
FROM nvidia/cuda:12.6.3-runtime-ubuntu22.04 AS runtime-cuda
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 65532 --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot
COPY --from=build /out/audiosilo-sidecars /usr/local/bin/audiosilo-sidecars
# Create /data owned by the runtime user BEFORE `USER nonroot` (see the CPU
# runtime stage) so a volume mounted at /data is writable by the non-root user.
RUN mkdir -p /data && chown nonroot:nonroot /data
USER nonroot
# The nvidia/cuda base already sets these; re-affirmed here for defensive clarity.
# `utility` is what makes the container toolkit inject nvidia-smi (CUDA detection).
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=compute,utility
EXPOSE 8090
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/audiosilo-sidecars", "serve", "--data", "/data", "--listen", "0.0.0.0:8090"]

# --- Stage 3b: slim runtime (CPU ASR, the default target) -----------------------
# debian-slim (not distroless/static) so the daemon can exec real ffmpeg/ffprobe:
# a downloaded static build is fragile on a static-only base, and bundling the
# tools means toolfetch never has to reach the network at runtime. Kept lean -
# --no-install-recommends and the apt lists are removed - and non-root. This is
# the LAST stage, so a bare `docker build .` produces this CPU image.
FROM debian:stable-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 65532 --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot
COPY --from=build /out/audiosilo-sidecars /usr/local/bin/audiosilo-sidecars
# Create /data owned by the runtime user BEFORE `USER nonroot`. A volume mounted
# over a missing image dir is created root-owned, so the non-root user could not
# write config/db/tools there; pre-creating it makes an anonymous OR named volume
# inherit writable ownership.
RUN mkdir -p /data && chown nonroot:nonroot /data
USER nonroot
EXPOSE 8090
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/audiosilo-sidecars", "serve", "--data", "/data", "--listen", "0.0.0.0:8090"]
