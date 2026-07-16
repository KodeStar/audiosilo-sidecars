# CLAUDE.md - AudioSilo Sidecars

Guidance for working in this repository. Keep this file updated as the codebase
evolves. This is the seventh repo in the AudioSilo workspace (`~/dev/audiosilo`) -
read the workspace [CLAUDE.md](../CLAUDE.md) first, plus
[audiosilo-meta/CLAUDE.md](../audiosilo-meta/CLAUDE.md) (the upstream metadata
database this tool contributes to) and its EXTRACTION.md / EXTRACTION-AUDIO.md
(the pipeline this tool automates).

## What this is

A standalone **contributor tool**: point it at an audiobook folder and it turns
that book into the community **characters/recaps sidecars** for
[meta.audiosilo.app](https://meta.audiosilo.app) - folder scan -> coverage check
-> ASR -> an agent pipeline (fact pass -> notes-only synthesis -> adversarial
spoiler audit) -> validated CC BY-SA sidecars -> a contribution (intake issue /
PR / keep-local). It packages the previously hand-run
`claude -p` / EXTRACTION-AUDIO.md process behind a Go daemon + embedded web UI so
anyone can help, with Claude or ChatGPT backends and subscription or API-key
auth. The design basis is the workspace plan (Context / Architecture /
Milestones).

It is a **client tool, not a server**: it reads the public meta.audiosilo.app API
(coverage/lookup, no auth) and produces PRs/issues. It never receives writes and
holds no community data of its own.

Module path: `github.com/kodestar/audiosilo-sidecars`. Code is **AGPL-3.0**
(matching audiosilo-server/meta). The sidecars it produces are CC BY-SA 3.0 (the
meta repo's content license) - never fabricated, own-words only; the copyright
rules in audiosilo-meta's AUTHORING.md / LICENSING.md are load-bearing for the
pipeline milestones.

## Model routing (every session follows this)

Sessions in this repo run a fixed division of labour between models:

- **Fable (the main session) is the orchestrator only.** It owns task
  decomposition, orchestration, design taste/direction, and final QA of every
  delegated piece. It **never writes feature code directly** - it reviews diffs,
  runs the gate, and sends work back when it falls short. Runs at **high** effort
  (do not escalate to xhigh/max). It may write orchestration artifacts itself:
  this file, briefs, commit messages.
- **Opus subagents do the implementation.** One subagent per task
  (`model: "opus"`); parallel when tasks touch disjoint files, sequential when
  one depends on another's output. Each subagent gets a self-contained brief
  (files, constraints, acceptance criteria) and must leave the gate green for
  the code it touched.
- **Token-hungry chores go to cheaper models** (Sonnet/Haiku): bulk codebase
  analysis/inventories, screenshot sweeps, log triage. They report findings back;
  they don't make design decisions.

## Build / test / gate

```sh
# Go side (from repo root) - the default build embeds a UI placeholder, so it
# needs NO Node toolchain and no generated files.
go build ./... && go vet ./... && go test -race ./... && golangci-lint run

# Frontend side (from web/) - Node 24 (export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH")
cd web && npx tsc --noEmit && npm run lint && npm run format && npm test

# Real-UI binary (embeds the built SPA via -tags embedui):
scripts/build-web.sh          # builds web/, syncs into internal/web/dist, builds bin/
./bin/audiosilo-sidecars serve # first run prints the one-time admin password ONCE
```

**Before a change is done, run all of the above for the side(s) you touched.**
golangci-lint is **v2** at a **green baseline** - fix new findings, don't widen
excludes (matches the server/meta repos' policy). Go 1.25; Node 24.

> Before adding code, read the workspace **[CODE-HEALTH.md](../CODE-HEALTH.md)** -
> Definition of Done + the recurring drift patterns. Especially: keep business
> logic out of the transport layer (`internal/api` is transport-only); every
> feature ships a test; security-critical code needs an allowed AND a denied test.

### Web build embedding (the `-tags embedui` seam)

`go:embed` cannot reach the repo-root `web/dist`, so the embed target is selected
by a build tag:

- **Default build (no tags)** embeds `internal/web/dist-placeholder/` (a tiny
  "run scripts/build-web.sh" page). This keeps `go build ./...` green on a fresh
  clone with no Node. The API is fully functional; only the UI is a placeholder.
- **`-tags embedui`** embeds `internal/web/dist/` (gitignored), which
  `scripts/build-web.sh` populates from the real `web/dist` build. This is the
  production/Docker binary.

This mirrors audiosilo-server's `-tags embedplayer`. Do not commit
`internal/web/dist/`; only the placeholder is tracked.

## Package layout

```
cmd/audiosilo-sidecars/   entrypoint: `serve` (default) + `version`; flags --data, --listen
internal/
  config/   config.yaml in <data>/ + AUDIOSILO_SIDECARS_* env overrides; Load/Save/Validate.
            M1 added library_roots (scan allow-list), metadata.base_url, scan.ffprobe_path;
            agent.concurrency is now live (scheduler agent lane). asr/agent-model routing
            stay typed stubs for later milestones.
  auth/     single admin password (argon2id, generated + printed once on first run),
            opaque SHA-256-hashed session tokens, a per-IP login rate limiter; the
            Store interface is storage-agnostic (MemStore for tests; the SQLite
            store.AuthStore in production - the M0 JSON store was removed in M1)
  secrets/  named secrets (anthropic/openai keys, github PAT) in the OS keychain
            (go-keyring) with a 0600 secrets.json fallback; read API is presence-only
  store/    SQLite (modernc, pure Go; single writer + WAL) + append-only migrations:
            books, stage_runs, progress, events (durable log, 30-day prune), rates
            (EWMA seed, create-only in M1), settings KV, sessions. Plain tested CRUD;
            AuthStore adapts it to auth.Store. Holds the SCHEDULING truth.
  state/    per-book pipeline state machine: table-driven states/lanes/transitions,
            CanStart/NextState guards, the audit fix-loop cap. Pure, no I/O.
  scheduler/ one wake-on-event goroutine over three lanes (ASR cap 1 / agent cap =
            config, series-locked / mechanical cap 2) with StubExecutor + _done/<stage>.json
            sentinels (the CONTENT truth) and crash reconcile. Pause/resume/retry/cancel/
            delete. Publishes book.state/stage.progress/queue.stats.
  metaops/  meta.audiosilo.app client (coverage/lookup, 1h TTL cache, graceful
            degrade) + async folder-scan job manager over audiosilo-meta pkg/scan +
            the library_roots PathAllowed check. stdlib HTTP + the meta module only.
  events/   SSE hub: Publish -> monotonic-id fan-out, ring-buffer replay from
            Last-Event-ID, ephemeral heartbeats, slow-subscriber eviction, optional
            durable-sink persister (feeds store.events)
  api/      transport-only HTTP: auth/system/settings/events + M1 scans/books/control
            handlers + middleware (bearer auth, allow-list CORS, security headers).
            NO business logic here.
  web/      go:embed of the SPA (build-tag selected) + SPA-fallback static serving
  server/   http.Server wiring, graceful shutdown, the startup banner
web/          the SPA: Vite + React 19 + TS + Tailwind v4 (npm, Node 24); dist/ is embedded
              src/lib/ holds pure, vitest-tested logic (apiClient, candidates, books,
              pipelineState, recentRoots, useEventStream); src/components/{library,running}/
              are the Library/Running tab views; components stay thin over src/lib
scripts/build-web.sh   build the SPA + embed it into bin/ (-tags embedui)
Dockerfile             multi-stage: node build -> go build (embedui) -> distroless
```

**Dependency direction** (transport-only rule): `server -> {api, auth, secrets,
events, config, store, scheduler, metaops, web}`; `api -> {auth, secrets, events,
config, store, scheduler, metaops}`; `scheduler -> {store, state, events}`;
`state` is pure. Handlers marshal DTOs and call into the injected packages; they
hold no logic (state transitions live in `state`, dispatch in `scheduler`).

## Conventions

- **`internal/api` is transport-only.** Handlers validate/route and call into
  `auth`/`secrets`/`config`/`events`. Keep logic in those packages so it stays
  unit-testable. Same rule as audiosilo-server/manager.
- **Every feature ships with a test.** Security-critical paths (auth resolve,
  rate limiter, CORS allow-list, settings-never-echo-secrets) require **both an
  allowed and a denied** test.
- **Secrets are never logged or echoed.** The one-time admin password is printed
  once in the first-run banner and never again; session tokens and API keys are
  stored only as hashes / in the keychain; the settings read API returns presence
  booleans, never values. Secrets never enter config.yaml.
- **Loopback by default.** `--listen` defaults to `127.0.0.1:8090`; auth is
  always on. A separately-deployed UI reaches the daemon cross-origin only via an
  explicit `cors_origins` allow-list.
- **Facts only in the pipeline (later milestones).** Sidecars are own-words,
  spoiler-gated, and verifiable; source audio/transcripts never enter this repo -
  only the derived CC BY-SA sidecars leave it. Follow audiosilo-meta's
  AUTHORING.md / EXTRACTION-AUDIO.md.
- **Hyphens, never em dashes** (workspace-wide rule), in docs, comments, UI copy,
  and generated text alike.

## Status / roadmap

Milestones from the workspace plan; each is shippable.

- **M0 (done):** repo skeleton, config, auth (first-run password + sessions +
  rate limit), secrets (keychain + 0600 fallback), the SSE event hub, the
  transport-only API, the embedded 4-tab web UI shell (Library/Running/Done are
  placeholders; Settings is real - password change + write-only secrets), and the
  Dockerfile stub. **Gate:** login local + remote-with-auth; SSE heartbeat visible
  in the UI liveness dot.
- **M1 (done):** Go side - SQLite store + migrations (`internal/store`), the
  per-book state machine (`internal/state`), the three-lane scheduler over stub
  executors with crash-resume sentinels (`internal/scheduler`), the folder scan
  (`audiosilo-meta pkg/scan`) + coverage/lookup client (`internal/metaops`), and
  the pipeline API surface (`POST /scans`, `GET /scans/{id}`, `POST /books`,
  `GET /books[/{id}]`, `POST /books/{id}/{pause,resume,retry,cancel}`,
  `DELETE /books/{id}`). Web side - the **Library tab** (folder path input +
  localStorage recent-roots, scan -> poll -> candidates table with per-dimension
  coverage badges [has/needed/unknown/unavailable], identity chips with provenance
  tooltips, exclude-already-covered toggle, select-all-visible, series-carryover
  gap hint, Process N books -> conflict-aware results) and a **minimal Running
  tab** (books list fetched on mount, live-updated from the SSE hub -
  book.state/stage.progress patches + a queue.stats header strip - with state
  chips colored by lane, status badges, and pause/resume/retry/cancel[confirm]/
  delete controls; the full board is M6). Non-trivial UI logic lives in pure,
  vitest-tested modules under `web/src/lib` (`candidates.ts`, `books.ts`,
  `pipelineState.ts`, `recentRoots.ts`); components stay thin. CI landed
  (`.github/workflows/ci.yml`: go + web jobs). The scheduler still runs stub
  executors that write `_done/<stage>.json` sentinels so the machine runs end to
  end; real executors arrive M2+. Gate verified: `go build/vet/test -race/
  golangci-lint` green, the full web gate green (`tsc`/lint/prettier/46 tests/
  build), and a live headless-browser drive (login -> scan -> candidates with
  coverage badges -> select 2 -> Process -> Running tab advancing to done live via
  SSE), plus the earlier Go smoke (pause/resume, kill -9 + resume with no
  duplicated stages, live coverage check against meta.audiosilo.app).
- **M2-M8 (planned):** toolfetch + audio (ffmpeg split), ASR backends
  (mlx-whisper + whisper.cpp), QA/spelling ports, the agent runner (claude +
  codex) with staged context dirs enforcing the invariants, the fact-pass +
  synthesis + audit loop, contribution (intake/PR/local), and packaging
  (GoReleaser + Docker matrix). See the plan for the full table.

M1 is built end to end (Library tab + a minimal Running tab over the stub
scheduler). Still **not built**: the **Done** tab (full board is M6), the Running
tab's richer board (stage timeline / ETA / cost, M6), and the pipeline's real
executors (audio/ASR/QA/agent/contribute) - the scheduler runs stub executors,
and the config `asr`/agent-model sections stay typed stubs. Note: `/system` still
reports the Running/Done tabs as `planned` (a Go-side label); the Running tab is
nonetheless fully rendered and usable. Keep this file honest as milestones land.
