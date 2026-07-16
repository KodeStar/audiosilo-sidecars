# Multi-stage build for the audiosilo-sidecars daemon + embedded web UI.
#
# Stage 1 builds the SPA (Node 24). Stage 2 embeds it into a CGO-free Go binary
# with `-tags embedui`. Stage 3 is a distroless static runtime.
#
# NOTE (M0): the image builds the daemon skeleton only. The extraction pipeline
# (ffmpeg/whisper/agent CLIs) is NOT bundled - later milestones add the tool
# dependencies and a heavier runtime variant (see the plan's Dockerfile.cuda).

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

# --- Stage 3: minimal runtime ---------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/audiosilo-sidecars /usr/local/bin/audiosilo-sidecars
EXPOSE 8090
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/audiosilo-sidecars", "serve", "--data", "/data", "--listen", "0.0.0.0:8090"]
