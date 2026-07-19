# AudioSilo Sidecars

A standalone contributor tool for [meta.audiosilo.app](https://meta.audiosilo.app):
point it at an audiobook folder and it produces the community **character sheets**
and **"story so far" recaps** (the spoiler-gated CC BY-SA sidecars) for that book,
then helps you contribute them back.

Under the hood it automates the AudioSilo extraction pipeline (local ASR -> an
agent fact/synthesis/audit pass -> validated sidecars) behind a small Go daemon
with an embedded web UI. Bring your own Claude or ChatGPT backend (subscription
or API key).

> Status: **feature-complete.** The full pipeline runs end to end - folder scan
> with coverage checks, chapter split, local ASR (mlx-whisper or whisper.cpp,
> auto-downloaded), transcript QA, the agent fact/synthesis/audit stages,
> validation, and contribution back to meta.audiosilo.app (prefilled intake
> issues by default, direct fork+PR, or keep-local export), with live boards,
> ETAs, and per-stage cost tracking in the web UI. Packaging is in place -
> tagged (`v*`) releases publish native binaries (GitHub Releases) and container
> images (GHCR, plus a `-cuda` GPU variant); see **Install** below. See
> [CLAUDE.md](CLAUDE.md) for the roadmap.

This is the seventh repository in the AudioSilo workspace. Code is licensed
**AGPL-3.0** (see [LICENSE](LICENSE)); the sidecars it produces are CC BY-SA 3.0,
the content license of the metadata database.

## Install

Tagged (`v*`) releases publish two things:

- **Native binaries** on the [GitHub Releases](https://github.com/KodeStar/audiosilo-sidecars/releases)
  page - one self-contained archive per OS/arch (Linux, macOS, Windows; amd64 and
  arm64), with the web UI embedded. Unpack and run `audiosilo-sidecars serve`.
  ffmpeg/ffprobe, the whisper.cpp CLI and the ASR models are not bundled - they are
  found on your `PATH` (or next to the binary) or auto-downloaded on first use into
  `<data>/tools`.
- **Container images** on GHCR:
  - `ghcr.io/kodestar/audiosilo-sidecars` (and `:latest`) - the CPU image, with
    ffmpeg bundled.
  - `ghcr.io/kodestar/audiosilo-sidecars:latest-cuda` - a GPU variant for NVIDIA
    hardware; run it with `--gpus all` and the NVIDIA container runtime, and the
    daemon auto-downloads the CUDA whisper.cpp build on first use.

To build from source instead, see below.

## Build and run

Requires Go 1.25 and (for the web UI) Node 24.

```sh
# Daemon + tests (no Node needed; the default build embeds a UI placeholder):
go build ./... && go test ./...

# Build the real web UI into the binary and run it:
scripts/build-web.sh
./bin/audiosilo-sidecars serve
# First run prints a one-time admin password ONCE - sign in with it at
# http://127.0.0.1:8090, then set your own password in Settings.
```

Flags: `--data DIR` (config/auth/secrets, default `~/.audiosilo-sidecars`),
`--listen HOST:PORT` (default `127.0.0.1:8090`, loopback only). `version` prints
the build version.

Long multi-book runs include a bounded, deterministic health monitor. Automatic
recovery, model-assisted diagnosis, model actions, and backend failover are separate
explicit opt-ins. See [Bounded batch supervisor](docs/BATCH-SUPERVISOR.md) for its
safety boundary, configuration, cost accounting, API/UI controls, and copied-data
testing procedure.

Agent capacity has separate controls for concurrent books and safe per-book
invocation fan-out. See [Agent capacity and per-book fan-out](docs/AGENT-CAPACITY.md)
for compatibility, supported stages, timing, liveness, cost, and safe testing.

### Frontend dev loop

```sh
cd web
export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH"
npm install
npm run dev        # Vite dev server; proxies /api to 127.0.0.1:8090
```

Run the daemon (`./bin/audiosilo-sidecars serve`) alongside `npm run dev` and the
SPA proxies API/SSE calls to it.

The Library page caches its latest successful folder-scan result in the daemon
data directory, so a daemon restart can restore the book list without walking the
library again. It is a snapshot: run **Scan** after changing files. UI tabs are
deep-linkable and refresh-safe with `?tab=library`, `?tab=running`,
`?tab=done`, or `?tab=settings`.

The Running tab separates post-transcription **Processing** from **ASR**. Current
workers appear first, followed by labelled agent, mechanical, transcription, and
corrective re-transcription queues in the daemon's actual dispatch order. Paused,
needs-attention, failed, and completed books remain in separate sections; a paused
book whose current stage is still winding down stays visible under its active worker.

### Docker

Use a published image (see **Install**) or build locally:

```sh
docker build -t audiosilo-sidecars .
docker run -p 8090:8090 -v sidecars-data:/data audiosilo-sidecars
```

For NVIDIA GPU-accelerated ASR, use the `-cuda` variant (or build it locally with
`docker build --target runtime-cuda -t audiosilo-sidecars:cuda .`) and pass
`--gpus all`:

```sh
docker run --gpus all -p 8090:8090 -v sidecars-data:/data \
  ghcr.io/kodestar/audiosilo-sidecars:latest-cuda
```

The container binds `0.0.0.0:8090`; the one-time password is printed to the
container log on first start.
