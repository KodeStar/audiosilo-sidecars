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
            M1 added library_roots (scan allow-list), metadata.base_url;
            agent.concurrency is now live (scheduler agent lane). M2 added tools.
            {ffmpeg_path,ffprobe_path,auto_download} - the SINGLE source of truth for
            tool paths (the ffprobe knob lives under tools.*; the folder scan uses
            the resolved path). M3a made asr.* live: backend
            (auto|mlx-whisper|whisper-cpp), model, language, whisper_cli_path. There
            is no asr.device knob (no backend honors an override yet; /system reports
            the DETECTED device). Changing asr.backend or the tool paths takes effect
            only on a daemon RESTART (the backend is resolved once at startup, unlike
            cors_origins, which the API re-reads live per request). agent-model
            routing stays a typed stub.
  toolfetch/ resolve ffmpeg/ffprobe (explicit path -> next to the binary -> $PATH ->
            HTTPS download from pinned hosts into <data>/tools when auto_download,
            extracted fully in-process (archive/zip + archive/tar over an xz decoder,
            per-entry name sanitization - no host tar), self-checked by running
            -version, no digest pinning); ported from
            audiosilo-server. Resolve() runs once at startup; a missing tool
            degrades gracefully (the audio stage fails that book, daemon keeps working).
            M3a added LocateBinary (the same lookup for whisper-cli) and EnsureModel
            (HTTPS ggml-model download, size floor + atomic rename + cache-hit skip).
  audio/    the mechanical audio stages: Inspect (ffprobe -> probe.json + normalized
            manifest.json; marker parsing + contiguity ported from audio_extract.py;
            single-file marker books AND multi-file "files" books) and Split (ffmpeg
            each chapter -> mono/16k FLAC under chapters/, resumable via temp+rename,
            per-chapter progress, ctx-cancel clean). Pure/tool-driven, no scheduler deps.
  asr/      the ASR backend abstraction (M3a): Backend{ID,Detect,EnsureReady,Transcribe}
            over a normalized Job (audio/outDir/chapter/prompt/language), producing RAW
            per-chapter output byte-for-byte. Two backends behind Select
            (auto|mlx-whisper|whisper-cpp): mlxwhisper (darwin/arm64; manages a pinned
            venv under <data>/tools/mlx-venv, model self-downloads via HF) and whispercpp
            (all platforms; resolves whisper-cli via toolfetch.LocateBinary, downloads
            ggml-large-v3-turbo into <data>/tools/models). One job at a time is the
            scheduler's job (Lane A cap 1); this package doesn't self-serialize. Never
            seeds the initial prompt with a guess. Gated live smoke: -tags asrlive.
  transcript/ the normalized transcript contract (audiosilo-transcript/v1) + Sanitize
            (NaN/Infinity->null, string-aware) + format-detecting adapters (openai-whisper
            /mlx AND whisper.cpp -ojf) + Complete (resume/skip test, ports
            transcript_is_complete) + writers (transcripts-json/ normalized,
            transcripts-text/ concatenated text). NEVER writes transcripts-raw/.
  scratch/  per-book DirSize gauge + Purge (removes chapters/, keeps durables),
            confined to the work root. Manual purge only in M2; auto-purge is M7.
            A purge also invalidates the split sentinel (scheduler.purgeInvalidatedStages)
            so a later retry re-splits rather than skipping into an empty chapters/.
  pipeline/ composite scheduler.Executor: routes inspecting -> audio.Inspect,
            splitting -> audio.Split, asr -> the per-chapter internal/asr loop
            (resumable: skip complete raws, delete+retry malformed, freeze each raw
            0444, write asr.json provenance, account scratch), sanitizing ->
            internal/transcript normalization; every other stage -> the stub (M4+
            replaces more; retranscribing is still a stub). Constructed in server.go
            with the toolfetch-resolved paths and the asr.Select-chosen backend. The
            sanitizing stage deliberately RE-DERIVES all chapters every run (cheap,
            idempotent, raw is the source of truth) rather than tracking per-chapter
            freshness. Missing tools PARK a book needs_attention (ASR unavailable, or
            ffmpeg/ffprobe unresolved) instead of hard-failing - a human-fixable
            startup precondition that Retry re-admits.
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
            config, series-locked / mechanical cap 2) over an injected Executor +
            _done/<stage>.json sentinels (the CONTENT truth) and crash reconcile.
            Pause/resume/retry/cancel/delete + PurgeScratch (reclaim chapters/ when
            done/paused/failed). Publishes book.state/stage.progress/queue.stats.
            M2 runs the pipeline composite executor (real inspect/split, stubs beyond).
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
Dockerfile             multi-stage: node build -> go build (embedui) -> debian-slim
                       runtime that apt-installs ffmpeg/ffprobe (so toolfetch never
                       auto-downloads in the container)
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
- **M2 (done):** the real mechanical audio stages. `internal/toolfetch` resolves
  ffmpeg/ffprobe (config path -> next to the binary -> `$PATH` -> HTTPS
  download into `<data>/tools`, self-checked by `-version`); `internal/audio` does ffprobe **inspect** (marker
  normalization + contiguity check ported from `audio_extract.py`, writing
  `probe.json`/`manifest.json`; single-file marker books and multi-file "files"
  books) and ffmpeg **split** to mono/16k FLAC (resumable, per-chapter progress).
  `internal/pipeline` wires a composite executor (inspecting -> audio.Inspect,
  splitting -> audio.Split, everything else -> the stub) into the scheduler.
  `internal/scratch` tracks per-book disk usage and reclaims `chapters/`
  (`PurgeScratch` + `POST /books/{id}/purge-scratch`, allowed only when
  done/paused/failed; the purge reserves the book id so a concurrent resume/retry
  can't race the chapter removal, and it drops the split sentinel so a retry
  re-splits); `scratch_bytes` rides on the book view and a daemon-total gauge +
  resolved tool paths surface on `/system` (shown in the Running header strip and
  a read-only Settings "Media tools" block). Non-contiguous markers (and a
  markerless file) set `MarkersContiguous=false`; the `markers_normalizing` stage
  then **parks the book needs_attention** with a clear message - automatic marker
  normalization is deferred to M5, so the book waits for a human rather than
  failing misleadingly at split. The Docker image bundles ffmpeg/ffprobe
  (debian-slim runtime), so the container never triggers a tool auto-download.
  Gate verified: full Go + web gates green, plus a live smoke (real 3-chapter
  m4b through inspecting -> splitting -> done, mid-split kill + resume without
  redoing chapters, purge drops the gauge, a non-contiguous book parks
  needs_attention).
- **M3a (done):** the ASR stage is real. `internal/asr` abstracts a `Backend`
  (`ID`/`Detect`/`EnsureReady`/`Transcribe`) over a normalized `Job` and
  `Select`s auto|mlx-whisper|whisper-cpp (auto = mlx on darwin/arm64 with python3,
  else whisper-cpp when a whisper-cli binary is found, else unavailable).
  **mlxwhisper** manages a pinned venv under `<data>/tools/mlx-venv` (the model
  self-downloads via Hugging Face on first run); **whispercpp** resolves
  `whisper-cli` (config path -> beside the binary -> `$PATH`; binary
  auto-download is deferred to **M3b**) and downloads `ggml-large-v3-turbo`
  into `<data>/tools/models`. `internal/transcript` owns the normalized
  **audiosilo-transcript/v1** contract: NaN/Infinity sanitizing, format-detecting
  adapters (openai-whisper/mlx AND whisper.cpp `-ojf`), the `Complete` resume test,
  and the derived `transcripts-json/`+`transcripts-text/` layers - the raw output
  stays byte-for-byte immutable (frozen `0444`) in `transcripts-raw/`. The pipeline
  `asr` stage runs a per-chapter resumable loop (skip complete raws, delete+retry
  malformed, freeze `0444`, write `asr.json` provenance, account scratch) staying in
  **Lane A (cap 1)** so only one book transcribes at a time (Metal contention);
  `sanitizing` derives the json/text layers. `/system` gains an `asr` block
  (backend/available/device/version). Gate verified: full Go race + lint + web gates
  green; a gated `-tags asrlive` mlx smoke (fresh venv 26s + real transcription);
  and a live daemon smoke (real 3-chapter m4b through inspect -> split -> asr(real
  mlx) -> sanitizing -> done, transcripts-raw `0444`, text non-empty, kill -9
  mid-asr on a second book resumes without re-transcribing the completed chapter).
- **M3b (in progress):** whisper.cpp binaries for non-Apple hardware. The
  **CI build matrix has landed** (`.github/workflows/whisper-binaries.yml`,
  manually dispatched: macOS Metal w/ embedded metallib + a real tiny-model
  transcription smoke, Linux CUDA w/ bundled cudart/cublas + `$ORIGIN` RPATH,
  Linux Vulkan, Linux amd64/arm64 + Windows CPU; ldd allow-list gates; flat
  archives + checksums.txt published as a `whisper-cpp-<ref>-<rev>` GitHub
  release - the asset names + tag are the distribution contract
  `toolfetch.WhisperCLIReleaseTag` consumes, publish-then-bump-const on
  upgrades). The toolfetch auto-download client is in flight.
- **M4-M8 (planned):** QA/spelling ports (M4), the agent runner (claude +
  codex) with staged context dirs enforcing the invariants, the fact-pass +
  synthesis + audit loop, contribution (intake/PR/local), and packaging
  (GoReleaser + Docker matrix). See the plan for the full table.

Still **not built**: the **Done** tab (full board is M6), the Running tab's richer
board (stage timeline / ETA / cost, M6), and the pipeline stages beyond sanitizing
(QA/agent/contribute) - the scheduler runs the composite executor whose remaining
non-mechanical stages (including `retranscribing`) are still stubs, and the config
agent-model section stays a typed stub. Auto-purge/startup-GC of scratch is M7 (M2
is manual purge only). `/system` reports Library/Running/Settings as `ready` and
only Done as `planned` (the Go-side tab labels). Keep this file honest as milestones
land.
