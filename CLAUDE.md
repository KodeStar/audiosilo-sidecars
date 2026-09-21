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
cd web && npm run typecheck && npm run lint && npm run format && npm test
# NOT `npx tsc --noEmit`: the root tsconfig is `"files": []` plus project
# references, and non-build-mode tsc ignores references - so that command checks
# ZERO files and exits 0 on any error. It let a broken type reach CI, where
# `npm run build` (tsc -b) caught it.

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

Compact map. The per-package invariants, incident history, and load-bearing
rules live in [PACKAGES.md](PACKAGES.md) - **read its entry for any package
you touch before changing it**.

```
cmd/audiosilo-sidecars/   entrypoint: `serve` (default) + `version`; flags --data, --listen
cmd/audiosilo-bench/      private corpus prep + isolated model-matrix benchmark runs + reports
internal/
  config/     config.yaml in <data>/ + AUDIOSILO_SIDECARS_* env; Load/Save/Validate
              (most agent.*/asr.*/tools.*/contribution.* knobs are restart-to-apply)
  toolfetch/  ffmpeg/ffprobe + whisper-cli + ggml model fetch/verify/cache under <data>/tools
  audio/      mechanical audio stages: ffprobe Inspect (marker parsing + coverage) +
              ffmpeg Split to mono/16k chapter FLACs (resumable, staged local source copy)
  asr/        ASR Backend abstraction: mlx-whisper (darwin/arm64) + whisper-cpp
  transcript/ the audiosilo-transcript/v1 contract: sanitize, adapters, Complete, writers
  ebook/      M9 epub front half: logical chapter universe from audiosilo-meta
              pkg/extract, chapter text + manifest writers, library .epub discovery
  scratch/    per-book disk gauge + Purge (chapters/), confined to the work root
  qa/         mechanical transcript-QA degeneration sweep (golden-tested Python ports)
  spelling/   corrections/spelling engines + candidate extraction + reference matching
  agent/      claude/codex headless-CLI runners, staging, retry policy, embedded prompts
  benchmark/  private provider-neutral post-ASR evaluation harness
  repair/     mechanical tail-clip/mid-clip splice + adoption machinery (no agent)
  pipeline/   composite scheduler.Executor wiring EVERY stage (the biggest PACKAGES.md
              entry - its invariants are essential reading)
  contrib/    GitHub-facing contribution: token source, REST client, composers, poller
  auth/       single admin password + hashed session tokens + login rate limiter
  secrets/    named secrets in the OS keychain (0600 fallback); presence-only reads
  store/      SQLite + append-only migrations; holds the SCHEDULING truth
  state/      pure per-book state machine + typed park codes
  eta/        pure ETA engine (EWMA unit rates + three-lane queue simulation)
  scheduler/  wake-on-event dispatch over three lanes + _done sentinels + crash reconcile
  supervisor/ health-tick babysitter: bounded, capped automatic recovery
  metaops/    meta.audiosilo.app client: coverage/lookup + search ladder, scan jobs,
              series glossary, library sightings (injected recorder -> the New view)
  events/     SSE hub (replay, heartbeats, durable sink)
  api/        transport-only HTTP handlers
  web/        go:embed of the SPA (build-tag selected) + SPA-fallback static serving
  server/     http.Server wiring, graceful shutdown, the startup banner
web/          the SPA: Vite + React 19 + TS + Tailwind v4; pure logic in src/lib
              (vitest-tested), thin components; scan state in scanStore.ts
scripts/build-web.sh   build the SPA + embed it into bin/ (-tags embedui)
Dockerfile             multi-stage: `runtime` (CPU, default) + `runtime-cuda` targets
.goreleaser.yml        native-binary release config (draft, embedui, archives+checksums)
```

**Dependency direction** (transport-only rule): `server -> {api, auth, secrets,
events, config, store, scheduler, metaops, pipeline, web}`; `api -> {auth, secrets,
events, config, store, scheduler, metaops}`; `scheduler -> {store, state, eta, events}`; `eta -> state` (pure, imported BY
scheduler - never the reverse);
`pipeline -> {audio, asr, transcript, ebook, qa, spelling, agent, repair, toolfetch,
scratch, secrets, fsutil, metaops, store, state, scheduler}` (metaops since M7's
contributing stage, and the spelling stage's series glossary); `agent`/`repair` are
leaf helpers (no scheduler/store deps; `repair -> qa` for the shared Python-compat
gram/repr helpers and the shared clip-floor predicate
`qa.ClipStartInRange`/`qa.ClipStartFloorSec`); `state` is pure; `metaops` never imports `store` - `server` injects the override
lookup/persist as adapters and the sightings recorder (`WithSightings`) as the
store itself (that interface is record + has-any, so nothing store-shaped crosses
it). Handlers marshal
DTOs and call into the injected packages; they hold no logic (state transitions
live in `state`, dispatch in `scheduler`).

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

**All milestones are DONE** (v0.1.0 released): M0 skeleton/auth/SSE/API/UI
shell; M1 store + state machine + scheduler + folder scan/coverage + pipeline
API + Library/Running tabs; M2 real inspect/split + toolfetch + scratch;
M3a/M3b real ASR (mlx-whisper + whisper.cpp incl. the whisper-binaries release
pipeline); M4 QA/spelling engine ports (golden-tested); M5 agent runner +
every remaining stage real; M6 Done board + richer Running board + ETA engine
+ typed park reasons; M7 contribution (issue/PR/local, needs-core flow,
poller, auto-purge); M8 packaging (GoReleaser binaries + GHCR CPU/CUDA
images); M9 ebook input (an EPUB is a first-class source - extracting ->
[chapter_mapping] -> the unchanged authoring tail; depends on audiosilo-meta
v0.8.0). Post-milestone rounds followed: UX/observability, spelling-cost,
reliability (the two bounded loops learned to ACCEPT; availability
self-resume via retry_at; per-book budget; superseded stage_runs), fact-pass
cost (bounded map-reduce), canonical-spelling (reference-match pre-pass +
series glossary), and library-matching + network-source (the metaops
ladder.go retrieval ladder + narrator evidence in the SHARED matcher [server
PR #40] + path hints, 510 unknown -> 45 over the live library; read-time
contribution folding into frozen scan verdicts; staged local split source,
source-IO serialization, one serial ASR slot, transient-EINTR retry,
time-bounded splits), and the Library "New" view (library_sightings +
read-time `is_new`/`first_seen_at`, attached by the API on every read and never
cached; the first recorded batch is the baseline). The
detailed milestone log - what each landed, the live incidents behind the
invariants, and the verification evidence - lives in [HISTORY.md](HISTORY.md);
read the entries for any stage or era you are working near.

Operative caveats surviving from that history:
- **Stub-sentinel books**: a book that advanced THROUGH a pre-M5 stub stage
  (including M4's pre-real `qa_sweep`) replays the stub `_done/<stage>.json`
  sentinel on resume - delete + re-enqueue such books. Books parked at the
  pre-M5 `markers_normalizing`/`qa_adjudicating` are safe (those parks wrote
  no sentinel).
- **whisper.cpp binaries ship on their own cadence** (`whisper-binaries.yml`,
  a separate release `toolfetch` consumes; publish first, then bump
  `toolfetch.WhisperCLIReleaseTag`) - never couple them into
  `release.yml`/`image.yml`.

Still **not built**: signed installers / a friendlier packaged client (a
possible follow-up per the meta EXTRACTION roadmap); a separately-deployable
UI-only image (scoped out of M8, deferred); contribution retraction (the
contributing stage submits sidecars but never retracts them - cancelling a
core_pending book leaves its already-opened add-work issue for a maintainer
to close); MOBI/AZW/PDF and DRM of any kind (deferred from M9). **Ebook
input SHIPPED in M9**; the former proposal in
[EBOOK-INPUT.md](EBOOK-INPUT.md) is kept only as the original design trail.
All four tabs report `ready` on `/system`. Keep this file (and
[HISTORY.md](HISTORY.md)/[PACKAGES.md](PACKAGES.md)) honest as work lands.
