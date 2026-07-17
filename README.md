# AudioSilo Sidecars

A standalone contributor tool for [meta.audiosilo.app](https://meta.audiosilo.app):
point it at an audiobook folder and it produces the community **character sheets**
and **"story so far" recaps** (the spoiler-gated CC BY-SA sidecars) for that book,
then helps you contribute them back.

Under the hood it automates the AudioSilo extraction pipeline (local ASR -> an
agent fact/synthesis/audit pass -> validated sidecars) behind a small Go daemon
with an embedded web UI. Bring your own Claude or ChatGPT backend (subscription
or API key).

> Status: **feature-complete, pre-release.** The full pipeline runs end to end -
> folder scan with coverage checks, chapter split, local ASR (mlx-whisper or
> whisper.cpp, auto-downloaded), transcript QA, the agent fact/synthesis/audit
> stages, validation, and contribution back to meta.audiosilo.app (prefilled
> intake issues by default, direct fork+PR, or keep-local export), with live
> boards, ETAs, and per-stage cost tracking in the web UI. Remaining before a
> first release: packaging (installers + Docker images). See
> [CLAUDE.md](CLAUDE.md) for the roadmap.

This is the seventh repository in the AudioSilo workspace. Code is licensed
**AGPL-3.0** (see [LICENSE](LICENSE)); the sidecars it produces are CC BY-SA 3.0,
the content license of the metadata database.

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

### Frontend dev loop

```sh
cd web
export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH"
npm install
npm run dev        # Vite dev server; proxies /api to 127.0.0.1:8090
```

Run the daemon (`./bin/audiosilo-sidecars serve`) alongside `npm run dev` and the
SPA proxies API/SSE calls to it.

### Docker

```sh
docker build -t audiosilo-sidecars .
docker run -p 8090:8090 -v sidecars-data:/data audiosilo-sidecars
```

The container binds `0.0.0.0:8090`; the one-time password is printed to the
container log on first start.
