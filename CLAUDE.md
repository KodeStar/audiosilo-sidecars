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
  toolfetch/ fetches the three external artifact families, all gated by
            tools.auto_download and confined to <data>/tools: ffmpeg/ffprobe static
            builds (explicit path -> next to the binary -> $PATH -> HTTPS download,
            in-process extraction w/ per-entry name sanitization, -version
            self-check; ported from audiosilo-server); the whisper-cli release
            binaries (M3b: EnsureWhisperCLI - platform+device asset table over the
            pinned WhisperCLIReleaseTag, sha256 verified against the release's
            checksums.txt, temp-dir extraction under a hard size budget, --help
            self-check, atomic install + .meta written last; CPU fallback on
            accelerated self-check failure, device-aware cache hit, stale-cache
            degrade when a refresh fails); and ggml ASR models (EnsureModel: size
            floor + .meta sidecar + atomic rename). LocateBinary is the shared
            no-download lookup. A missing artifact degrades gracefully (the stage
            parks/fails that book; the daemon keeps working).
  audio/    the mechanical audio stages: Inspect (ffprobe -> probe.json + normalized
            manifest.json; marker parsing + contiguity ported from audio_extract.py;
            single-file marker books AND multi-file "files" books) and Split (ffmpeg
            each chapter -> mono/16k FLAC under chapters/, resumable via temp+rename,
            per-chapter progress, ctx-cancel clean). Pure/tool-driven, no scheduler deps.
  asr/      the ASR backend abstraction (M3a/M3b): Backend{ID,Detect,EnsureReady,
            Transcribe} over a normalized Job (audio/outDir/chapter/prompt/language),
            producing RAW per-chapter output byte-for-byte. Two backends behind
            Select (auto|mlx-whisper|whisper-cpp): mlxwhisper (darwin/arm64; manages
            a pinned venv under <data>/tools/mlx-venv, model self-downloads via HF)
            and whispercpp (all platforms; whisper-cli resolves local-first via
            toolfetch.LocateBinary, else the toolfetch auto-download cache - Detect
            is optimistic when auto-download can supply a binary, EnsureReady(ctx)
            performs the fetch + downloads ggml-large-v3-turbo; an explicit
            whisper_cli_path that does not resolve is a loud error). One job at a
            time is the scheduler's job (Lane A cap 1); this package doesn't
            self-serialize. Never seeds the initial prompt with a guess. Gated live
            smoke: -tags asrlive.
  transcript/ the normalized transcript contract (audiosilo-transcript/v1) + Sanitize
            (NaN/Infinity->null, string-aware) + format-detecting adapters (openai-whisper
            /mlx AND whisper.cpp -ojf) + Complete (resume/skip test, ports
            transcript_is_complete) + writers (transcripts-json/ normalized,
            transcripts-text/ concatenated text). NEVER writes transcripts-raw/.
  scratch/  per-book DirSize gauge + Purge (removes chapters/, keeps durables),
            confined to the work root. Manual purge only in M2; auto-purge is M7.
            A purge also invalidates the split sentinel (scheduler.purgeInvalidatedStages)
            so a later retry re-splits rather than skipping into an empty chapters/.
  qa/       M4: the mechanical transcript-QA degeneration sweep, a faithful Go port
            of the historical Python detectors (qa_sweep/cross_segment/
            within_segment/multi_loop/tail_rate scans - thresholds and per-detector
            chapter-0 asymmetries are CONTRACT, golden-tested against 2 real books).
            Reads transcripts-json/ (+ transcripts-repaired/ for multi-loop); writes
            qa_report.json (stable enum contract for the M5 adjudicator/UI) +
            qa_report.md (byte-compatible with the Python report). Report.Clean()
            drives the QAClean branch; the retranscribe queue = wph outliers +
            mid-chapter loops. Loud errors on manifest/transcript divergence,
            wrong-schema files, and an empty transcript set.
  spelling/ M4: the corrections/spelling ENGINES ported from apply_corrections/
            check_corrections/generate_spellings/check_first_use.py. Engine-vs-data
            split: per-book data is corrections.json + spellings.json in the work
            dir (M5 agents generate them). Rules apply in array order via regexp2
            (RE2 lacks the lookbehinds the historical rules need; $1 replacement
            syntax, validated gate-compatible at load); Occurrences is a lookaround
            boundary scan, never \b (the d'Daston apostrophe pitfall). Check's four
            gates guard the historical forgeries (Owalyn gate 3, phantom nobles
            gate 4, the Book-2 cascade gate 1); GenerateSheets emits the
            CHUNK_ENDS-gated spoiler-safe sheets. Attestation sources are purely
            data (reference_files) - nothing implicit. Not yet wired into pipeline
            stages (spelling_research/correcting stay stubs until M5).
  pipeline/ composite scheduler.Executor: routes inspecting -> audio.Inspect,
            splitting -> audio.Split, asr -> the per-chapter internal/asr loop
            (resumable: skip complete raws, delete+retry malformed, freeze each raw
            0444, write asr.json provenance, account scratch), sanitizing ->
            internal/transcript normalization, qa_sweep -> the internal/qa sweep
            (writes both reports, branches on Report.Clean()); qa_adjudicating
            deliberately PARKS needs_attention (a dirty book must not skip human
            adjudication; automatic adjudication is M5); every other stage -> the
            stub (M5+ replaces more; retranscribing is still a stub). Constructed in
            server.go with the toolfetch-resolved paths and the asr.Select-chosen
            backend. The sanitizing stage deliberately RE-DERIVES all chapters every
            run (cheap, idempotent, raw is the source of truth) rather than tracking
            per-chapter freshness. Missing tools PARK a book needs_attention (ASR
            unavailable, or ffmpeg/ffprobe unresolved) instead of hard-failing - a
            human-fixable startup precondition that Retry re-admits.
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
  metaops/  meta.audiosilo.app client (coverage/lookup, capped 1h TTL caches,
            graceful degrade) + async folder-scan job manager over audiosilo-meta
            pkg/scan + the library_roots PathAllowed check. Coverage resolves
            asin -> isbn -> a fuzzy title-search fallback scored by
            audiosilo-server's pure-stdlib pkg/match (Coverage carries matched_by
            "asin"|"isbn"|"search"|"manual" + work_title provenance). Scans STREAM:
            the manager drives pkg/scan's OnProgress/OnBook hooks, books appear
            incrementally (identity provisional until done - the corroborated,
            sorted final list replaces the array), coverage resolves in a bounded
            pool gated by precomputed identity fingerprints (a stale worker can
            never clobber a fresh verdict), and List() serves job summaries
            (running + last 10 finished) so a reloaded UI reattaches. Persisted
            candidate_overrides (hide / manual work match, keyed by the CANONICAL
            absolute source_path - scan roots and override paths are resolved via
            the same helper) are applied at scan time and reflected live on
            completed jobs via read-time patches; OverrideService owns the
            validate -> resolve -> persist -> reflect workflow (store injected as
            a PersistFunc, so metaops still never imports store). Deps: stdlib
            HTTP + the meta module + audiosilo-server/pkg/match.
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
              pipelineState, recentRoots, useEventStream, scanStore); src/components/
              {library,running}/ are the Library/Running tab views; components stay
              thin over src/lib. The Library tab's scan + selection state lives in
              scanStore.ts - a module-level external store (useSyncExternalStore)
              owning the 700ms poll loop, so tab switches (AppShell unmounts
              panels) and reloads (GET /scans reattach) never lose a running scan;
              sign-out calls scanStore.reset(). API calls key books by the
              daemon-computed absolute source_path (NEVER a client-side join);
              the relative path is display/selection only.
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
- **M3b (done):** whisper.cpp binaries for non-Apple hardware, zero manual
  installs. The **CI build matrix** (`.github/workflows/whisper-binaries.yml`,
  manually dispatched: macOS Metal w/ embedded metallib + a real tiny-model
  transcription smoke, Linux CUDA w/ bundled cudart/cublas + `$ORIGIN` RPATH,
  Linux Vulkan, Linux amd64/arm64 + Windows CPU; ldd allow-list gates; flat
  archives + checksums.txt) publishes a `whisper-cpp-<ref>-<rev>` GitHub
  release - the asset names + tag are the distribution contract
  `toolfetch.WhisperCLIReleaseTag` consumes (publish first, then bump the
  const). The **auto-download client** (`toolfetch.EnsureWhisperCLI`, gated by
  `tools.auto_download`) picks the asset by platform + detected device,
  verifies its sha256 against the release's checksums.txt (a missing line or
  mismatch adopts nothing), extracts to a temp dir with a hard total size
  budget, self-checks `--help`, installs atomically under
  `<data>/tools/whisper-cpp/` and writes a `.meta` (tag/asset/sha/fallback)
  LAST. Policies: an accelerated asset failing its self-check falls back once
  to the CPU asset (sticky until a tag bump); the cache hit is device-aware
  (installing a GPU driver later re-downloads the accelerated build); a failed
  refresh degrades to the previously-installed binary; an explicit
  `asr.whisper_cli_path` that does not resolve is a loud error, never silently
  replaced. asr's whisper-cpp `Detect` is optimistic when auto-download can
  supply a binary; `EnsureReady` (now `EnsureReady(ctx)` - backends use their
  own data dir) does the real fetch per book and the pipeline PARKS
  (needs_attention, actionable message) on its failure instead of hard-failing.
  The `v1.9.1-1` release is live with all 6 assets; the client was live-smoked
  against it (download + verify + self-check + zero-network cache hit).
  internal/audio also now sorts multi-file books with audiosilo-meta's exported
  `scan.NaturalLess` (meta PR #33) instead of a private copy - chapter numbers
  spoiler-gate contributed sidecars, so the shared comparator is load-bearing.
- **Library UX round (done, post-M3b):** the first-real-use feedback batch.
  Matching quality: coverage now falls back from asin/isbn to a fuzzy
  title-search against meta.audiosilo.app scored by audiosilo-server's
  pkg/match, with matched_by/work_title provenance shown in the UI (a tagless,
  ASIN-less folder book matches by title alone). Scans stream: audiosilo-meta
  pkg/scan gained OnProgress/OnBook hooks (meta PR #35), so the Library tab
  shows per-folder progress and incremental candidates. Manual match: a per-row
  Match modal searches the public meta API (GET /api/v1/meta/search proxy) and
  persists the pick; Hide/Unhide persists too (candidate_overrides, migration
  0003, keyed by canonical absolute source_path; "Show hidden (n)" re-shows).
  The tab-switch bug is fixed at the root: all scan/selection state lives in
  web scanStore.ts (module-level external store owning the poll loop), with
  GET /scans powering reload reattach. POST /books candidates + overrides key
  on the daemon-computed absolute source_path (the old relative-path flow
  silently broke PathAllowed); books.work_id persists any matched work for
  later pipeline stages. Side quests: the server module became properly
  fetchable (server PR #39 - a testdata apostrophe made every version's module
  zip invalid, so pkg/match is a normal require, no replace directive), and
  config.yaml's listen key is honored (the --listen flag default no longer
  clobbers it). Gate verified: full Go + web gates, an API smoke against the
  live meta service (search-fallback match, hide, manual match, trailing-slash
  canonicalization), and an 8/8 headless-Chromium drive (login -> scan ->
  provenance -> tab-switch persistence -> hide/unhide -> match modal -> reload
  reattach -> process). Done-tab cost columns wait for M5/M6 cost capture.
- **M4 (done):** the QA/spelling Go ports. `internal/qa` ports the six
  degeneration detectors (wph |z|>2.5 outliers w/ sample stdev; >=3
  identical-normalized segment runs split end-fade [>=85% position] vs
  MID-CHAPTER; low-confidence <0.5 stats; cross-segment 6-gram THRESHOLD=5;
  within-segment >=8x; multi-loop every-gram w/ word-set dedup +
  repaired-layer preference; tail-rate TAIL_WORDS=12/MAX_WPS=4.5) - the
  thresholds, per-detector chapter-0 asymmetries and Python-truthiness quirks
  are contract, preserved verbatim and documented in code. `internal/spelling`
  ports the corrections engine (ordered rules via dlclark/regexp2 - stdlib RE2
  cannot express the historical lookbehinds; `$1` replacement syntax,
  gate-compatibility validated at load), the four check_corrections gates
  (LHS-zero, RHS-present, RHS-attested vs the data-driven reference_files
  union, phantom-noble scan - the Owalyn-forgery and d'Daston regressions are
  unit tests), the CHUNK_ENDS spoiler-gated spellings sheets (Gate 1
  zero-occurrence, Gate 2 note-names-later-term), and check_first_use; per-book
  data is corrections.json/spellings.json in the work dir, which M5 agents will
  generate. The pipeline's `qa_sweep` stage is REAL (qa_report.json/.md +
  QAClean branch); `qa_adjudicating` parks needs_attention until M5. Golden
  tests (env-gated `AUDIOSILO_EXTRACTION_DIR`, skip in CI, ~/extraction strictly
  read-only via temp copies, numbers-only in-repo expectations) replay 2
  historical books: qa_report.md byte-prefix-identical for HW05 + RLF03, all 84
  HW05 corrected chapters byte-identical with per-rule counts matching the
  historical corrections.log, Check passes, sheet rows/unresolved/cluster
  gating identical; `scripts/export-extraction-data.py` converts the historical
  embedded Python data tables to the JSON contracts at test time
  (sys.dont_write_bytecode - the extraction dir stays untouched). Known
  pre-release caveat: a work dir whose `qa_sweep` sentinel was written by the
  pre-M4 STUB replays `qa_clean=true` on resume (only books parked exactly at
  qa_sweep before the upgrade) - delete + re-enqueue such books.
- **M5-M8 (planned):** the agent runner (claude + codex) with staged context
  dirs enforcing the invariants, marker normalization + QA adjudication +
  retranscription going live, the fact-pass + synthesis + audit loop, per-stage
  model/tokens/cost capture (feeds the M6 Done board), contribution
  (intake/PR/local), and packaging (GoReleaser + Docker matrix). See the plan
  for the full table.

Still **not built**: the **Done** tab (full board is M6), the Running tab's richer
board (stage timeline / ETA / cost, M6), and the pipeline stages beyond `qa_sweep` -
`qa_adjudicating` deliberately parks (M5), and the agent/contribute stages plus
`retranscribing` are still stubs; `internal/spelling` is a ported, golden-tested
engine not yet wired to any stage (spelling_research/correcting go live in M5, when
agents generate its corrections.json/spellings.json). The config agent-model section
stays a typed stub. Auto-purge/startup-GC of scratch is M7 (M2 is manual purge
only). `/system` reports Library/Running/Settings as `ready` and only Done as
`planned` (the Go-side tab labels). Keep this file honest as milestones land.
