# HISTORY.md - AudioSilo Sidecars milestone log

The detailed milestone-by-milestone history (M0-M9 + the post-milestone rounds):
what each landed, the live incidents behind the invariants, and the
verification evidence. Moved verbatim from CLAUDE.md's "Status / roadmap"
section so the always-loaded guide stays lean. CLAUDE.md keeps the compact
current status, the operative caveats, and the not-built list.

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
- **M5 (done):** the agent runner and every remaining pipeline stage. `internal/agent`
  is the runner abstraction over headless **claude** and **codex** CLIs (prompt on
  STDIN, `--output-format json` / codex JSONL, usage capture, typed rate-limit/
  not-available errors, `Select` auto|claude|codex with explicit path knobs, `ModelFor`
  per-stage routing, `RunWithRetry` invalid-output + rate-limit policy), a per-attempt
  **staged context dir** (`_runs/<stage>-a<n>/`, 0444 copied inputs + `out/`, `Harvest`
  with traversal + size-cap guards) that enforces the process invariants, and
  `go:embed` **prompts** (one template per stage, each vendoring the forbidden-source /
  own-words / hyphens-only rules, plus a vendored audiosilo-meta `AUTHORING.md`).
  `internal/repair` is the mechanical tail-clip + full-chapter adoption machinery.
  Every stage is now real: markers_normalizing / qa_adjudicating / spelling_research /
  fact_pass / synthesizing / auditing / fixing run the agent through a shared `runAgent`
  driver; retranscribing / correcting / validating are mechanical (ASR+repair / the
  spelling engine / canonicalize + audiosilo-meta n-gram). Cost is captured per agent
  invocation (migration 0004 columns + `AddOpenStageRunUsage`) and surfaced on `/system`
  (agent block), settings GET/PUT (agent config, restart-to-apply), the web Settings
  **Agent** card, and the Running-tab book detail cost line. **Park conditions** (all
  Retry-re-admittable, mirroring the media-tools park): the agent backend is unavailable
  (`AgentUnavailableMsg`); markers_normalizing gets a not-confident verdict; qa_adjudicating
  does not converge after 3 rounds; correcting's spelling Check fails a gate; an agent's
  output fails validation after the retry budget; the audit->fix loop hits `MaxFixAttempts`
  (3). **Stub-sentinel caveat**: the pre-M5 `markers_normalizing`/`qa_adjudicating` stages
  PARKED needs_attention and wrote NO sentinel, so a book parked at either re-runs the real
  stage on resume - the replay risk does NOT apply to them. The caveat is only for a book
  that advanced THROUGH the generic pre-M5 STUB at a stage that is now real (a book taken to
  `done` via stubs, or the M4 `qa_sweep` case its own note documents): its stub
  `_done/<stage>.json` makes the now-real stage skip on resume, so delete + re-enqueue such a
  book. Gate verified: full Go race + lint green, the
  full web gate green, a full-machine integration test (a real non-contiguous m4b through
  markers_normalizing -> a dirty-QA -> adjudicate-retranscribe -> clean re-sweep ->
  spelling/fact/synthesis -> an audit-fail fix loop -> done, asserting the cleared-sentinel
  re-runs and per-stage usage) plus the focused per-stage/invariant/loop tests.
- **M6 (done):** the **Done board**, the **richer Running board**, and the **ETA
  engine**. `internal/eta` (pure) owns per-stage EWMA unit rates (chapter/chunk/book
  units, alpha 0.3, seeded from the historical extraction metrics, persisted in the
  `rates` table), book ETA (rate x remaining units over the optimistic mainline derived
  from `state.MainlineNext` - loops are not predicted), and queue ETA (a greedy
  three-lane simulation with injected LaneCaps, `state.HoldsSeriesLock` series locks,
  and retranscribe-first then breadth-first-across-series ASR ordering). Rate
  observation is stage-owned: each stage
  returns a `StageResult.RateSample` (units actually processed this run + productive
  seconds measured after setup, agent rate-limit backoff excluded via the runner's
  slept-time return) so resumes, first-run tool/model downloads, and backoff never
  contaminate learned rates; progress reporting is display-only and starts at the
  resume baseline. The scheduler recomputes on each dispatch pass (idle-gated) and
  publishes deduped `eta.update` SSE (`queue_seconds` null when idle);
  `eta_seconds`/`started_at` ride on bookView. **Typed park reasons**: `state.ParkCode`
  (10 codes) flows ParkWithCode -> books.park_code (migration 0005; invariant enforced
  in the store) -> the `book.state` event -> per-class affordance hints in the UI.
  New endpoints: `GET /books/{id}/sidecars` (metaserve-shaped preview envelope,
  composed in pipeline, injected into api) and `GET /books/{id}/events` (durable log);
  both have allowed + denied tests. The **Done tab is real** (`/system` reports it
  `ready`): rows with finished date/total cost/scratch, per-stage cost columns
  (stage/model/tokens/cost/elapsed - closes Library-UX feedback item 6), a sidecars
  preview modal rendering characters/recaps like meta.audiosilo.app (vendored
  `expressive.ts`; spoiler accordions closed by default), a "Local only" contribution
  chip (until M7), purge + delete. The **Running tab** gained the stage-chip timeline
  (mainline + off-mainline insertion), elapsed + ETA chips (hidden while
  paused/parked), a Queue-ETA strip stat, a live per-book event log in the details
  expansion, and park-code hints. `books.chapters` records the manifest count after
  inspect; pre-M6 books fall back to their progress-row totals. Gate verified: full Go
  + web gates green after /simplify (14 applied) and /code-review (10 verified
  findings applied), plus a live smoke (real say-synthesized m4b live on the board -
  timeline/ETA/elapsed/log, agent-unavailable park with the typed hint, kill -9
  resume without re-transcribing, done-book cost table/preview/purge through the real
  API, SSE `eta.update` frames, idle null queue ETA).
- **M7 (done):** **contribution**. The `contributing` stage is real: it reconciles
  the sidecars' workSlug placeholder to the real meta work slug (books.work_id ->
  asin/isbn lookup; NEVER a fuzzy auto-adopt - wrong-work attachment is a spoiler
  hazard), rewrites the files' `work` field + replaces the staging source with one
  community provenance entry identifying the source audiobook edition (local
  ASIN/ISBN first, else narrator+runtime matched against the resolved work's
  recordings, with a work ref only when edition evidence is unavailable) +
  re-canonicalizes in place, skips upstream-covered dimensions (already_covered
  rows), then submits
  per contribution.mode: **issue** (default - prefilled add-characters/add-recaps
  intake issues rendered to metaissue's exact form-markdown contract, secret-gist
  fallback for >60k bodies, label verification with a maintainer-hint note when
  GitHub drops labels for non-collaborators), **pr** (fork + branch
  sidecars/<slug>-<id> + contents at data/works/<shard>/<slug>/ + one PR;
  crash-resume reuses an existing branch/PR/file-sha instead of 422ing), or
  **local** (export to <data>/export in repo layout + the Done tab's zip
  download). Every submit is resume-idempotent via the contributions rows.
  **Needs-core flow**: a work missing upstream parks core_needed with a prefilled
  contrib/core_proposal.json (narrators/authors/title from the scan, language from
  asr.json, runtime from manifest.json - omit-never-guess); the Running tab's
  modal completes it -> POST contribute/core opens the add-work issue (per-book
  mutex + row-reuse make it double-submit-proof) -> core_pending -> the poller
  reads the merged intake PR's files for the REAL slug (deterministic, no
  lookup-guessing), persists it, and re-admits; a lagging metaserve artifact
  cannot oscillate the book (a merged core row makes the stage trust the recorded
  slug on a 404). **Live status**: the poller advances rows submitted -> pr_open
  (intake/issue-<n>) -> merged/closed and publishes contrib.update; the Done
  tab's chip is live (Issue open/PR open/Merged/Closed/Local only + per-kind
  links; pre-M7 done books show the legacy Local-only chip). Auto-purge on
  reaching done + async reservation-guarded startup GC close the scratch loop.
  GitHub credentials: secrets GitHubPAT else `gh auth token`, never argv/logs.
  Companion meta-repo change (PR #43, merged): intake.yml runs on `labeled`
  (outcome labels excluded) so a maintainer applying the routing label admits a
  label-dropped API issue. Note: scratch_bytes measures the whole work dir, so a
  purged done book keeps its durable footprint (sidecars/manifest/asr.json) and
  the startup GC re-runs its idempotent purge each boot - harmless. Gate
  verified: full Go + web gates green after /simplify (16 applied) and
  /code-review high (9 verified bugs + 5 cleanups applied, incl. submitPR
  crash-resume idempotency, SubmitCore double-submit safety, the core_pending
  oscillation guard), plus a live smoke against a scripted local fake GitHub
  (contribution.api_base_url) + live meta reads: 17/17 assertions - both intake
  issues with exact form bodies, poller submitted -> pr_open -> merged with SSE
  frames, crash-resume posting no duplicate issues, the full needs-core round
  trip (park -> modal -> add-work issue -> merged-PR slug resolve -> readmit ->
  done), local export + zip download, 401s on unauthed endpoints, auto-purge +
  startup GC, and a headless-Chrome drive of chips/preview/modal/settings.
- **M8 (done):** packaging. On every `v*` tag two pipelines run, mirroring
  audiosilo-server's split:
  - **Native binaries** - `.goreleaser.yml` (v2, CGO-free cross-compile for
    linux/darwin/windows x amd64/arm64, `-tags=embedui`, version via
    `-X main.version`) + `.github/workflows/release.yml` (a **draft** GitHub
    Release; the workflow runs `scripts/build-web.sh` to populate the gitignored
    `internal/web/dist` for the embed BEFORE GoReleaser, so no `before` hook and
    the clean-tree check stays happy; `dist/` is gitignored). Archives + checksums
    only (no deb/rpm/systemd - it is a user-run, keychain-using tool, not a system
    service). ffmpeg/ffprobe/whisper.cpp/models stay runtime-fetched.
  - **Container images** - `.github/workflows/image.yml` (a two-variant matrix to
    GHCR): both variants build from ONE `Dockerfile` via multi-stage `--target`
    (shared web/go build stages, defined once) - `runtime` (CPU, `:latest`) and
    `runtime-cuda` (`:latest-cuda`, `nvidia/cuda:*-runtime` base). The `-cuda` tags
    come from the metadata-action `suffix` flavor; both legs are `linux/amd64` (arm64
    is a follow-up). The CUDA whisper-cli is **toolfetched at runtime** (it bundles
    its own cudart/cublas; the image only needs the driver injected by the
    nvidia-container-toolkit) - NOT baked in. Hardening fix landed here: both runtime
    stages `mkdir -p /data && chown nonroot:nonroot /data` before `USER nonroot`, so
    a volume mounted at `/data` is writable (a volume over a missing image dir is
    root-owned - the daemon could not write config/db there and failed to boot).
  - **whisper.cpp binaries stay decoupled**: they ship on their own cadence via
    `whisper-binaries.yml` (a separate release `toolfetch` consumes), so a whisper
    rebuild never forces a daemon re-release. This is deliberate - do not couple them
    into `release.yml`/`image.yml`.
  Gate verified: full Go gate green; `goreleaser check` clean + a single-target
  snapshot build produced a working versioned binary; the CPU image builds and boots
  end-to-end (real embedded UI served, one-time password printed, whisper-cpp ASR
  available, `/api/v1/system` 401s unauth); the `runtime-cuda` target passes `docker
  buildx build --check` (a full GPU build + live NVIDIA transcription is manual -
  needs NVIDIA hardware, matching the whisper-binaries CUDA leg).

- **Post-M8 UX + observability round (done):** first-real-use feedback after the
  v0.1.0 release.
  - **Library tab**: a candidate **search** box (case-insensitive AND-token match
    over title/authors/series/narrators/asin/isbn plus the book's relative PATH -
    real libraries encode identity in the folder layout the tags lack) and
    **series-order sort**
    (grouped by series name, then parsed series position, then title/path) as the
    default candidate order (`web/src/lib/candidates.ts` `searchCandidates`/
    `sortBySeries`, `scanStore` `search`).
  - **Faster large-SMB scans**: audiosilo-meta `pkg/scan` was parallelized (bounded
    concurrent directory walk instead of a serial ReadDir chain) and gained an
    `OnWalk` callback; the require is bumped to consume it and the Library
    "Scanning folders..." line now streams live "N folders, M books found" during
    the walk (`internal/metaops` `walk_dirs`/`walk_groups` on `ScanProgress`,
    `web/src/lib/scanStatus.ts`).
  - **Running tab**: a book **duration** chip (total audio length from inspect,
    persisted as `books.duration_sec`, migration 0007, via a `SetBookDuration`
    gauge that does not bump `updated_at`) and scheduler-owned queue placement.
    `QueueSnapshot` serves `queue_group`/`queue_bucket`/`queue_position`/
    `queue_active` on book views and the full `queue_books` snapshot on
    `queue.stats`; buckets preserve the exact order inside independently dispatched
    agent, mechanical, full-ASR, and corrective-ASR queues without inventing a
    global serial order across workers. The UI renders separate **Processing**
    (post-ASR) and **ASR** sections with current workers first, followed by labelled
    queues and paused/needs_attention/failed/done sections
    (`web/src/lib/books.ts` `sortBooks`/`groupRunningBooks`). A cooperatively paused
    in-flight stage remains in its active bucket until the worker exits. This avoids
    treating every book with a non-empty lane as active - waiting stages have a lane
    too.
  - **Pipeline observability**: the stage reporter is now
    `scheduler.StageReport{Progress, Note}`; `Note` publishes a durable `stage.note`
    event. A **heartbeat** (`agent.Request.Heartbeat`, ticked from inside
    `runCLI`'s select loop) emits "<stage>: still running (Nm elapsed)" every 60s
    ONLY while the agent subprocess is genuinely alive (a real liveness signal,
    silent during rate-limit backoff), and stages emit a **work-set descriptor on
    entry** (e.g. "re-transcribing 2 chapters: 2, 3"). The per-book **log** is now
    fully reachable: `store.ListEvents` gained a `beforeID` keyset cursor,
    `GET /books/{id}/events?before_id=` pages, and the Running details render a
    scrollable log with a **Download log** button (`web/src/lib/bookLog.ts`
    `fetchAllEvents`/`logToText`).

- **Post-M8 spelling-cost round (done):** made the spelling_research stage
  complete reliably instead of burning its retry budget (the first real book spent
  230K output tokens / $16.70 / 51 min failing it 3x). Three changes: (1)
  **patch-style validation retries** in `agent.RunWithBackoff` - the staged cwd +
  `out/` persist across attempts, so a retry now instructs the agent to read its
  prior output and fix/DELETE exactly the failing entries rather than regenerate
  the stage from scratch (applies to every agent stage); (2) the **candidate
  extractor** (`spelling.ExtractCandidates` -> `spelling_candidates.json`, see the
  package table) staged INSTEAD of the full ~600KB transcript - ~156KB, deterministic,
  capped-never-silently; for a series book only the small `spelling-refs/prior-*`
  files are staged (the predecessor's corrected texts stay work-dir-only for the
  gate corpus); a zero-candidates result over a >5000-word corpus fails loudly
  before the agent spends anything; (3) **prompt + validator hardening** -
  spelling.md forbids rules targeting unlisted forms, explains the gates'
  deriveBase "'s"-stripping trap (the real book-1 killer: `Leafs Crossing ->
  Leaf's Crossing` attests the nonexistent literal "Leaf Crossing"), tells the
  agent that deleting a failing rule + marking the name unresolved is always
  acceptable, and the validator now mechanically rejects **dead rules**
  (`spelling.DeadRules` against the original layer, naming each dead pattern in
  the retry feedback) - previously a dead rule whose RHS was attested elsewhere
  passed all four gates as silent under-correction.

- **Post-M8 reliability round (done):** made large books complete WITHOUT a human
  babysitter - after the first end-to-end success (31 chapters), 6 of 6 real books
  failed: 5 parked `qa_no_converge`, one 90-chapter book parked
  `fix_loop_exhausted` after $62.45 (audit rounds fix:4 -> 1 -> 1 -> 1, blocker 0,
  every round's fixes correctly applied, each fresh audit finding ~1 genuinely new
  small defect - an unreachable fix==0 bar on a large book), and one stranded
  `agent_unavailable` mid-run by a transient `exec.LookPath` blip. The hand-run
  process had a HUMAN as the terminating condition at exactly the two loops; this
  round taught them to ACCEPT. Five changes (details in the package table): (1)
  the **directed tail repair** - the dominant qa_no_converge cause was
  `LocateTailRun` silently no-opping on short tail repeats BEFORE consulting the
  agent's `clip_start_sec`, so the adjudicator's recourse was dead code; now the
  override drives a run-less, health-gated cut (TAIL-REPAIRED verdict), and true
  no-ops surface as `clips_unlocatable` + a note; (2) the **durable
  qa_accepted.json ledger** - accepts survive rounds, so stale-layer re-flags stop
  burning a paid re-verification of the same chapters every round; (3) **audit
  trajectory acceptance** - blockers always fail, but a converging non-growing
  actionable count with <= 2 FIX findings at round >= 2 (including BLOCKER -> FIX
  severity improvement) writes `audit_accepted.json`, takes ONE final fix
  round, and runs a bounded verifier over the accepted findings (no known defect
  ever ships merely because mechanical validation is clean; residuals recorded +
  surfaced on the contribution note); Retry on `fix_loop_exhausted`
  grants a genuinely fresh loop (fixing count + trajectory reset - previously it
  re-parked after one wasted audit); (4) **availability self-resume** -
  `books.retry_at` (migration 0008) + a scheduler auto-readmit pass for
  `agent_unavailable`/`agent_rate_limited`, reset-time parsing with a 5min floor,
  transient-NotAvailable in-process retries, and a human-only preflight park when
  no backend is configured; (5) **cost containment** - `agent.book_budget_usd`
  (default 75) parks `budget_exceeded` before more spend, and Retry SUPERSEDES
  stage_runs instead of deleting them so spend history (and the budget) survive.
  Auditing stays on opus (it is the public-contribution quality gate; acceptance
  bounds its rounds), and the audit model's
  per-round history lives in work-dir JSON, not the DB.

- **Post-M8 fact-pass cost round (done):** replaced the serial rolling knowledge
  rewrite with bounded map-reduce. Each chunk now extracts only chapter-attributed
  delta facts from its own corrected chapters and spoiler sheet; chunks run in
  parallel up to `agent.max_agents_per_book`; one notes-only assembly produces a sub-2500-word
  `knowledge-final.md`. A shared invocation semaphore caps parallel chunks plus all
  other agent stages globally, and both backends run in stateless/ephemeral mode with
  stage-sized tool/turn bounds. Chunk facts are individually resumable, so a crash
  keeps every completed paid extraction. Synthesis uses reveal-safe roster snapshots
  and an explicit coverage pass. The audit convergence re-entry now performs targeted
  semantic verification instead of accepting on mechanical validation alone.

- **M9 (done): ebook input.** An EPUB is now a first-class source. The pipeline
  gains a second FRONT HALF - `queued -> extracting -> [chapter_mapping] ->
  fact_pass` - feeding the unchanged authoring tail, so an epub skips inspect,
  split, ASR, sanitize, QA sweep, QA adjudication and spelling research entirely.
  All of those exist to reconstruct clean, correctly-spelled, correctly-chaptered
  text from audio, and an epub already has it.
  Measured over a 33-book library: 26 books route straight to `fact_pass`, 5 need
  the mapping agent, 2 park. Pieces: `books.kind`/`ebook_path`/`words`
  (migration 0011, invariant enforced in the store like park_code);
  `internal/ebook`; the `extracting` and `chapter_mapping` stages (the latter
  cloned from `markers_normalizing`, including its FREE deterministic re-derivation
  and its not-confident park); a kind-aware final-text layer so the tail reads
  either kind; and epub discovery in the Library scan.
  Load-bearing decisions, each guarding a SILENT failure: the new states sit at
  order 11/12 (at the front, `scheduler.queueGroup` would have rendered every
  ebook in the ASR section of a lane it never enters); `classifyEbookEdges`
  replaces the audio edge classifier, whose word-and-duration rule degenerates to
  word-only on a durationless manifest and would exclude a short opening chapter,
  shifting every reveal and recap one chapter early; `ngramCheck` now FAILS when it
  checked nothing, instead of reporting a vacuous pass on the one gate standing
  between the pipeline and republishing the author's prose; and the purge is
  kind-aware, because an ebook's text is the copyrighted source and must not
  outlive the derivation while audio's `transcripts-corrected/` is durable.
  Depends on audiosilo-meta **v0.8.0**, which added toc-anchor splitting (12 of
  the 33 books put several chapters in one spine document, and emitting one file
  per document silently merged them), a much wider chapter-label vocabulary (the
  old one recognized ZERO labels across the corpus), and `ReadMetadata`.
  Deferred: MOBI/AZW/PDF, DRM of any kind, and using an epub as a spelling
  reference for the audio path.

- **Post-M8 canonical-spelling round (done):** stopped the pipeline propagating its
  own mishearings across a series. A whole Wandering Inn run published `Torrin` for
  Toren, `Floss` for Flos, `Terriarch` for Teriarch, `Laken Goddard` for Godart,
  `prognogator` for Prognugator and `Valsaif` for Valceif - every book internally
  consistent, so nothing ever looked wrong. Two causes, both now addressed. (1) The
  agent's only signal is intra-transcript disagreement, and a name misheard the SAME
  way every time produces none; `spelling.BuildReferenceMatches` adds a deterministic
  edit-distance pre-pass (`spelling_reference_matches.json`) that reports transcript
  forms no reference source spells that way. (2) The series carryover was actively
  REINFORCING the error - book 5's corrected text (book 6's gate-3 attestation
  corpus) held `Torrin` 369x / `Floss` 1135x and `Toren`/`Flos` zero, and its ledger
  recorded both as canonical, so gate 3 would happily attest the mistake and the
  prompt told the agent the carried ledger wins. `metaops.SeriesGlossary` now pulls
  the canonical names the community database records for the series' OTHER volumes
  into `spelling-refs/series-glossary.txt` (already an allowed `reference_files`
  citation and already in the dry-run corpus - no validator change), and
  `ReferenceSource.Authority` ranks it ABOVE the carryover in both the Go engine and
  spelling.md's evidence order. The prompt now also states that transcript frequency
  is not evidence of spelling. Note the marker-title path helps only sometimes: some
  volumes ship `Chapter 9: Toren`, others a bare `001..027` table.

- **Post-M9 library-matching + network-source round (done):** the first drive
  against a real 1147-book SMB library, in two halves landed as two lines of
  work (the matching half merged as PR #5; the network-source half followed on
  main).
  **Matching** - 510 books resolved no coverage, and the dominant cause was
  RETRIEVAL, not scoring: the upstream FTS index matches the words it is given,
  so a decorated shelf title ("Supermage : Rise To Omniscience, Book 1")
  retrieves nothing even when the work exists under a clean title.
  `internal/metaops/ladder.go` is the fix: 9 ordered, de-duplicated query
  shapes per unresolved book (raw title first and ALWAYS - the length floor
  applies only to DERIVED rungs, so a book called "It" still searches;
  punctuation-normalized; CleanTitle; pre-subtitle; post-separator tail;
  trailing-volume stripped; title+author; folder-leaf and parent-dir PATH
  hints), early exit at the first rung `match.Best` accepts, a
  leaf-scoring contradiction guard, and a VOLUME VETO so widened retrieval can
  never accept a sibling volume. Narrator evidence rides `match.Query`/
  `match.Book` into the SHARED matcher (audiosilo-server matching overhaul,
  server PR #40, module pin bumped) - shelves that credit the narrator as the
  author resolve in one Best call, no local mirror of the person-name rule.
  Replayed over the live library: 510 unknown -> 45 unmatched (91% rescued).
  Coverage staleness: scan verdicts are frozen at scan time, so books this
  daemon contributed kept reporting "needed" - GET /scans and the book view
  fold the contributions table in at read time (`store.LandedCoverage` +
  `Coverage.ApplyContributed`, gated on work-id agreement so a contribution
  merged under a different work never stamps another work's badges; the read
  degrades to unpatched badges on a transient store error rather than breaking
  the 700ms poll). The Library search matches the relative path, "exclude
  already covered" also drops books the pipeline itself finished, and the
  manual-match picker retries punctuation-normalized queries.
  **Network-source reliability** - macOS's SMB client wedges under concurrent
  or rapid random access, which one 60-chapter marker book can trigger alone.
  Split now STAGES the source m4b into the work-dir split-source/ directory
  (one sequential 4MB-buffered copy, atomic rename, size-freshness-checked
  reuse across a failed split's retry, skipped when every chapter is already
  cut, scratch-registered so a failed split's copy is reclaimable, DEGRADES to
  direct reads when staging fails) so every per-chapter ffmpeg open is local;
  a narrowly-classified transient EINTR from a FUSE/Nextcloud source
  (`audio.IsTransientSourceErr`) retries the resumable split in-place; the
  split runs under the shared stage heartbeat (`runWithStageHeartbeat`,
  generalized from the ASR one) with both halves TIME-BOUNDED (per-chapter
  max(10min, 2x audio duration), staging max(15min, size-derived) - mirroring
  asrChapterDecodeBound) so a wedged mount is a loud failure, not an invisible
  hang; and the split RateSample excludes staging and backoff. The scheduler
  serializes source-reading mechanical stages to one worker (the state.Def
  SourceIO column via `state.ReadsSource`) while work-dir-only stages keep the
  second mechanical slot, and the ASR lane's separate corrective slot is GONE:
  full-book and corrective transcription share the one serial MLX worker
  (overlapping decodes pushed the second process into the OOM killer on
  unified-memory Macs), corrective work taking priority when the slot frees.
  The ETA queue simulation models both (`LaneCaps.MechanicalSourceIO`).
  Supporting fixes: `agent.isRateLimit` no longer treats a bare "429" as a
  rate limit (RFC3339 fractional seconds and usage counters contain those
  digits - a timestamped log line misclassified a validation failure) while
  still catching the JSON/`status_code=429` shapes; `runCLI` bounds Cmd.Wait
  with a 5s WaitDelay so a detached CLI helper holding the inherited stdout
  pipe cannot hang a finished stage; the supervisor grants a 90s
  `processExitGrace` (it must outlast the 60s stage-run heartbeat cadence it
  is measured against) before classifying a just-exited child as a
  disappeared process, and `collectArtifactStatuses` trusts an OPEN stage run
  over a stale book-state snapshot. Prompt hardening: audit/audit_verify/fix
  now state that the ledger's `Unresolved / do-not-publish-clean` section is
  the one hard override - a fact note can support a name the ledger omits but
  can never promote a surface form the ledger explicitly marks unresolved -
  pinned by a prompts drift-guard test alongside auditJSONPrompts.
  `validateMarkersManifest` accepts adjacent per-marker exclusion declarations
  whose gap-free union covers one coalesced unmapped span (`exclusionsCover`).
  Gate: /simplify (4 angles) and /code-review --fix (8 angles, adversarially
  verified) applied over the round; full Go and web gates green.
- **Library "New" view round (done, 2026-09-21):** the Library tab shows hundreds
  of folders, and nothing recorded when any of them appeared - so "what is new
  since I last looked?" was unanswerable. Migration 0012 adds `library_sightings`
  (source_path PRIMARY KEY, first_seen_at, last_seen_at, acknowledged_at,
  baseline), path-keyed with no FK to the rebuildable book index like
  candidate_overrides, so a sighting survives an enqueue, a delete and a rescan.
  `metaops.ScanManager` records every candidate of a COMPLETED scan through an
  injected `SightingRecorder` (`WithSightings`: record + has-any, so the store
  satisfies it directly and metaops still never imports store), BEFORE the job
  reports done - a client stops polling at done, so its last poll must already see
  the flags. Nothing sighting-shaped rides the job snapshot or the scan cache:
  `first_seen_at` and `is_new` are attached by the API on every read, exactly like
  `pipeline_book`. The BASELINE rule is the upgrade
  guard: the first batch ever recorded is stored as baseline and is never new,
  seeded at startup from the restored scan cache (stamped with the cached job's
  started_at) and otherwise by the first completed scan - without it an upgrade
  would report the user's whole library as new on first open. `is_new` is derived
  at READ time (`metaops.ComputeIsNew(book, baseline, acknowledged)`: not
  baseline, not acknowledged, no pipeline_book, not hidden; called only for a path
  that HAS a sighting row, so an unrecorded path is never new) beside the existing
  pipeline_book join in `GET /scans/{id}`, because three of those four inputs
  change without a rescan and a stored flag would be stale until the user walked
  their library again. `POST /api/v1/library/sightings/acknowledge` (204, unknown
  paths ignored) is the Dismiss action. Web side: scanStore holds a
  `view: 'new' | 'all'` filter defaulting to `new`, with a Dismiss bulk action
  over the new endpoint. Gate: Go build/vet/`test -race`/golangci-lint green, with
  store record/bump/baseline/acknowledge/has tests, a ScanManager recording +
  cache-seed test, the ComputeIsNew table, and allowed + denied route tests.

- **License: the community layer to CC BY-SA 4.0 (2026-09-22).**
  KodeStar/audiosilo-meta-community relicensed the community layer to **CC
  BY-SA 4.0** upstream (audiosilo-meta commit 4a06b1a1, 2026-08-21, first
  tagged in v0.13.0), and its intake bot began rejecting every submission from
  this tool with `characters: /license: value must be 'CC-BY-SA-4.0'`. Every
  license string this repo emits or teaches moved to 4.0 in one pass: the
  `sidecarLicenseContent` constant the local validator enforces on both sidecar
  files, the contributing stage's PR body, `contrib`'s `itemCCBySALicense`
  checkbox text (re-verified byte-for-byte against
  `.github/ISSUE_TEMPLATE/add-characters.yml` / `add-recaps.yml`, along with
  the other item labels the composer mirrors - all still exact), the four
  embedded agent prompts (authoring/synthesis/audit/fix), the compose/roundtrip
  goldens and the done-board fixtures, and README/CLAUDE.
  **The go.mod pin could NOT follow.** No tag carrying the 4.0 enum is
  consumable as a Go module: from v0.9.0 on, audiosilo-meta's `data/` tree is
  ~1.6 GB, over the go command's 500 MiB module-zip ceiling, so
  `go get github.com/kodestar/audiosilo-meta@v0.13.0` (or any later tag) fails
  with "module source tree too large" from both the proxy and direct. v0.8.0
  stays pinned, which is safe for the sidecar contract: v0.8.0's
  `characters.schema.json` and `recaps.schema.json` are byte-identical to
  v0.15.0's, so no new or renamed required field exists - `common.schema.json`'s
  `license_content` enum is the ONLY sidecar-relevant change (its other
  additions, a `libex-import` source type and the `genre`/`genre_list` defs, are
  core-record vocabulary this tool does not emit). Consequence for the drift
  guard: `TestSidecarConstantsMatchUpstreamSchema` still asserts hard equality
  for the caps, role/scope enums and QID pattern, but the license branch now
  tolerates exactly one known-stale value (`stalePinLicenseContent`) and fails
  on anything else, and a new `TestSidecarLicenseIsTheCommunityLayerValue` pins
  the emitted string to 4.0 so a silent revert cannot ship. Two
  `validateSidecars` cases assert the retired 3.0 value is rejected on
  characters.json AND recaps.json. Unblocking the pin is an upstream job: give
  audiosilo-meta a nested `data/go.mod` so the data tree drops out of the module
  zip, then tag. Gate: full Go gate green; no web changes.

- **Go 1.26 + the audiosilo-meta pin unstuck (2026-09-25).** audiosilo-meta PR
  #2366 made its `data/` tree a nested module, so a meta tag's module zip is a
  few MiB again and the pin moved from v0.8.0 to **v0.17.0**, the first tag
  carrying that change. Taking it forces meta's Go floor (`go 1.26.0`) here:
  go.mod, the Dockerfile's `golang:1.26` build stage, README and CLAUDE (CI
  reads `go-version-file: go.mod`). What broke against the new pkg/*: (1)
  `model.Shard` is gone - upstream's tree is range-packed, so a path no longer
  names a record. The contribution paths that spell per-record paths keep
  their behaviour through `contrib.LegacyShard`, a verbatim copy; they target
  a layout upstream retired (and a repo the sidecar layer left on 2026-08-21),
  which is a separate redesign, recorded in CLAUDE.md. (2) `extract.NGram` now
  identifies a bare sidecar by EVERY schema-required key and refuses one
  missing any - which would have failed the validating stage, whose contract
  is that only IO fails it. `ngramGate` reports a missing key (or a non-object
  file) as an ERROR finding and skips the n-gram check until the fixer repairs
  the record; `sidecarRequiredKeys` is pinned to the schemas' `required` by
  the drift test. One test fixture gained the keys. The drift guard is STRICT
  again: `stalePinLicenseContent` and
  `TestSidecarLicenseIsTheCommunityLayerValue` are gone, since the upstream
  enum is now CC-BY-SA-4.0 and plain equality holds. Gate: full Go gate green.
