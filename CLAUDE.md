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
            cors_origins, which the API re-reads live per request). M5 made agent.*
            LIVE: backend (""|claude|codex), concurrency, claude_path/codex_path,
            timeout_minutes, and the per-stage claude/openai model maps (keys are agent
            stage names). Validate rejects an unknown backend, an unknown model-map key,
            or timeout_minutes < 1; Default() seeds the claude map. M7 added
            contribution.{mode [issue|pr|local], repo, auto_purge, poll_minutes,
            api_base_url} (restart-to-apply; api_base_url exists for tests/GHE;
            Validate rejects an unknown mode, a non-owner/name repo, poll_minutes < 1,
            a non-http(s) api_base_url). The reliability round added
            agent.book_budget_usd (default 75; 0 in yaml normalizes to the default, so
            set a very large value to effectively disable; Validate rejects negative;
            env AUDIOSILO_SIDECARS_AGENT_BOOK_BUDGET_USD; restart-to-apply like the
            rest of agent.*) - the per-book agent-spend cap runAgent enforces.
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
            confined to the work root. Reclaimed manually (purge-scratch), by M7's
            auto-purge when a book reaches done (contribution.auto_purge, default on),
            and by the startup GC (async, reservation-guarded, done books only).
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
            data (reference_files) - nothing implicit. Wired into the pipeline in M5:
            the spelling_research agent generates corrections.json/spellings.json and
            the correcting stage runs Apply/Check/GenerateSheets over them.
            Post-M8 spelling-cost round: ExtractCandidates (candidates.go) distills
            the transcript into spelling_candidates.json - a deterministic, capped
            (never silently: Truncated + a stage note) shortlist of likely proper
            nouns (multi-word capitalized phrases, non-initial capitalized singles,
            non-dictionary capitalized singles at count>=3, lowercase variants, rare
            non-dictionary lowercase; Unicode-aware tokens incl. d'Aston internal
            capitals) with counts/chapters/up-to-2 snippets - which the stage stages
            to the agent INSTEAD of the ~600KB transcripts-text/ (the agent may only
            write rules targeting listed forms). commonwords.go is a generated
            (scripts/gen_commonwords.py) license-safe common-word list feeding the
            non-dictionary heuristics. DeadRules (deadrules.go) is the validator-side
            dead-rule scan: a rule whose pattern matches nothing in the ORIGINAL
            (pre-Apply, repaired-preferred) layer is rejected with the pattern named
            - Check's four gates miss that shape whenever the RHS is attested
            elsewhere, and it is deliberately NOT a fifth Check gate (Check is a
            contract-frozen golden-tested port).
  agent/    M5: the agent-runner abstraction (Runner{ID,Detect,Run} over a normalized
            Request/Result/Usage) + claude and codex headless-CLI backends (prompt on
            STDIN never argv, --output-format json / codex --json JSONL, usage capture,
            typed RateLimitError/NotAvailableError), Select (auto|claude|codex, explicit
            path knobs, loud on an unresolvable explicit path), ModelFor per-stage
            routing, RunWithRetry (invalid-output <= 2 PATCH-style retries: the staged
            cwd + out/ persist across attempts, so the retry prompt carries the
            validator error and instructs the agent to fix/delete the offending
            entries in its prior out/ files, never regenerate from scratch;
            rate-limit backoff), staging.go (per-attempt staged dir under
            _runs/, 0444 copied inputs + out/, Harvest with traversal + size-cap guards),
            and go:embed prompts/ (one template per stage + a vendored authoring.md from
            audiosilo-meta). The child env injects an API key from secrets that NEVER
            appears in argv/logs/errors (tested). Reliability round: RunWithBackoff also
            retries a transient NotAvailableError (15s, 60s - the claude CLI auto-update
            briefly breaks exec.LookPath, which stranded a real book mid-fact_pass);
            RateLimitError carries a best-effort ResetAt via ParseResetTime (the
            |<epoch> suffix within (now, now+48h], or "resets at H[:MM] am/pm" as the
            next host-local occurrence).
  repair/   M5: the mechanical tail-clip + adoption machinery (ports the historical
            tail_clip_check/adjudicate_tails/build_repairs): ClipAndSplice (locate the
            tail loop, cut+re-transcribe the window prompt-free, health-check, rotation-
            adjudicate FABRICATED/BENIGN/CLIP-REDEGENERATED, splice into
            transcripts-repaired/ + repairs.log/tail_verdicts.json) and AdoptFresh
            (never-blindly-adopt full-chapter plausibility). No agent; injected a
            ClipCutter (ffmpeg) + a Transcribe func so it stays unit-testable. The
            re-transcription (full-chapter AND clip) runs NoContext (asr.Job.NoContext)
            so a context-conditioned repetition collapse cannot replay identically.
            ClipSpliceRequest.StartOverrideSec accepts an agent-supplied window start
            (0 = derive as before, byte-identical); the cut clip file is keyed on
            chapter+effective window start (t%03d-<start>.flac), so a relocated window
            forces a fresh cut instead of reusing the prior window's audio. A window whose
            clip_start already carries a CLIP-REDEGENERATED verdict (within ~1s) UNDER THE
            SAME decode params is SKIPPED without cutting/re-transcribing
            (SkippedKnownFailed), so a re-queued identical tail_clip is free instead of
            minutes of ASR - the verdict records a DecodeTag, so a legacy verdict written
            under different (context-conditioned) params never blocks the chapter's one
            fresh NoContext attempt. MID-CHAPTER interior repair (ClipAndSpliceWindow, for
            an interior loop with real narration resuming AFTER it): the agent supplies a
            bounded [clip_start_sec, clip_end_sec] window (EndOverrideSec), which
            snapWindow snaps OUTWARD to segment edges (no straddling content lost), the
            stage cuts+re-transcribes prompt-free/NoContext, ClipHealthy gates it (no
            rotation-adjudication - there is no closing line), and SpliceWindow splices the
            fresh window BETWEEN the intact head (before start) and tail (after end).
            TailVerdict.ClipEnd (>0) records the window as a MID-REPAIRED (or mid
            CLIP-REDEGENERATED) verdict; the mid clip file is m%03d-<start>-<end>.flac
            (both bounds in deciseconds, dot-free, distinct from the tail t-prefix). The
            known-failed skip and the residual auto-accept both key on ClipEnd (a mid
            verdict never blocks a tail re-attempt, and vice versa). LIMITATION (known,
            deferred): the repair model is ONE clip per chapter - a single repaired
            overlay + one verdict per chapter (MergeTailVerdict upserts by chapter) + a
            chapter-level resume guard (tailClipAlreadyDone). So a chapter needing TWO
            windows (two separate loops, or a mis-covered mid window that must be widened)
            cannot compose: a second clip re-splices from the ORIGINAL transcript (losing
            the first) or is skipped as already-done. Mitigated by prompting the agent to
            bound mid windows GENEROUSLY (a mid_clip cannot be refined) and to use
            retranscribe for a multi-loop chapter; the endSec is clamped to the chapter
            duration. A proper fix (per-window verdicts + splice composition) is a
            follow-up. The reliability round added the DIRECTED (run-less) tail path:
            when LocateTailRun finds no loop (a SHORT tail repeat - a 3x phrase is below
            the 6-gram locator's reach) but the plan supplied clip_start_sec,
            clipAndSpliceDirected cuts [override, chend+2] anyway, health-check-gated
            like the MID path (no rotation-adjudication), writing a TAIL-REPAIRED
            verdict (ClipEnd 0, so auto-accept/known-failed behave like a located tail);
            a degenerate override past chend-1 is the Unlocatable no-op, never an error.
            ClipResult.Unlocatable() distinguishes the true no-op (no loop AND no
            override), which the stage buckets as clips_unlocatable + a note naming the
            chapters so the adjudicator knows to supply clip_start_sec next round.
  pipeline/ composite scheduler.Executor: routes inspecting -> audio.Inspect,
            splitting -> audio.Split, asr -> the per-chapter internal/asr loop
            (resumable: skip complete raws, delete+retry malformed, freeze each raw
            0444, write asr.json provenance, account scratch), sanitizing ->
            internal/transcript normalization, qa_sweep -> the internal/qa sweep
            (writes both reports, branches on Report.Clean()). M5 made EVERY remaining
            stage real: markers_normalizing/qa_adjudicating/spelling_research/fact_pass/
            synthesizing/auditing/fixing are AGENT stages (staged dir + rendered prompt
            + validated outputs via the shared runAgent driver, usage recorded onto the
            open stage_run after every invocation), while retranscribing/correcting/
            validating are MECHANICAL (ASR+repair / spelling engine / canonicalize+ngram).
            M7 made contributing real (contrib_stage.go: slug reconcile -> skip-if-
            covered -> submit per contribution.mode, resume-idempotent via the
            contributions rows; export.go composes the download zip + core-proposal
            JSON injected into api) - EVERY stage is now real. The load-bearing
            invariants live in
            the staging (synthesizing/auditing dirs hold NO transcripts, fact_pass chunk
            dirs hold no chapter beyond the chunk's end, spelling reference_files are
            restricted to the daemon-staged carryover). Constructed in server.go with the
            toolfetch-resolved paths, the asr.Select-chosen backend, and the
            agent.Select-chosen runner (nil when no CLI resolved). The sanitizing stage
            deliberately RE-DERIVES all chapters every run (cheap, idempotent, raw is the
            source of truth) rather than tracking per-chapter freshness. Missing tools or
            an unavailable agent backend PARK a book needs_attention (a human-fixable
            precondition Retry re-admits) instead of hard-failing. The qa_adjudicating ->
            retranscribing -> qa_sweep loop has three convergence guards beyond the
            maxQARounds (5) round cap: (1) a progress-based STALL park - the retranscribing
            stage INCREMENTS a retranscribe_stalled counter marker whenever a repair round
            splices AND adopts nothing (no real progress) and removes it on any progress;
            qaAdjudicate parks ParkQANoConverge (naming the stuck chapters) only at count
            >= 2 - a count of 1 grants the agent exactly ONE resolution round, because a
            no-progress round produces precisely the feedback (clips_unlocatable notes,
            known-failed skips, kept retranscribes) its terminal accept-or-direct decision
            needs (live-verified: the one-round park fired before the adjudicator could
            ever use the unlocatable feedback). The adjudicator's staged transcripts cover
            qa.AllowedChapters (every disposable chapter), not just the required
            FlaggedChapters - a textless tail-rate-only chapter made the agent queue
            conservative clips it could not verify. This REPLACED an earlier report+ledger sha256 fingerprint, which
            thrashed on the exact incident it was meant to catch: a re-degenerating tail
            clip rewrites tail_verdicts.json every round (each CLIP-REDEGENERATED verdict
            relocates its clip_start), so the fingerprint changed every round, the fixed
            point never fired, and the book burned its whole round budget (~$1.5/round). The
            marker is DELETED on ANY qa_adjudicating park (the stall park and the round-cap
            park) and on the done==0 reset (a Retry/purge-rewind must not inherit a stale
            marker), so a Retry always gets exactly ONE fresh agent round before it can
            re-park. A mid_clip splice increments spliced (it reuses the tail buckets), so
            an interior repair counts as progress too; (2) a WIDENED residual auto-accept -
            tailOnlyChapters (via spanCovered) accepts a repaired chapter whose
            cross-segment / multi-loop hits are residuals the recorded splice window covers:
            the window is [clip_start, clip_end] for a MID splice (both bounds constrain the
            span within +/-15s) or [clip_start, +Inf] for a TAIL splice (only the start,
            with a position>=95% fallback when the hit has no usable time). A MID-CHAPTER
            multi-loop is covered ONLY by a recorded MID window (never a tail window); a mid
            window with an untimed hit is conservatively NOT covered; (3) plan
            clip_start_sec/clip_end_sec (per tail_clip/mid_clip entry) feeds the repair
            known-failed skip above. wph outliers, within-segment hits, non-end-fade runs, a
            MID-CHAPTER multi-loop NOT covered by a mid window, and spans that straddle
            mid-chapter into the tail still always disqualify a chapter from residual-only.
            The repair re-transcription is decode-tagged
            (retranscribe/decode_params marker): a stale pre-NoContext fresh raw is discarded
            so the chapter is re-transcribed under the current params rather than reused, and
            a free known-failed skip is excluded from the stage's rate sample. The
            reliability round added (4) a DURABLE accepted-chapters ledger
            (qa_accepted.json): every accept (agent's and auto-accept's) persists and is
            mechanically re-accepted in later rounds without re-invoking the agent - most
            detectors read the stale unrepaired layer, so repaired chapters re-flag
            forever and were being re-verified at full agent cost every round. The ledger
            is deliberately NOT cleared by the done==0 reset or Retry (repairs only touch
            planned non-accept chapters, so accepted decisions stay valid). The AUDIT
            loop likewise now terminates by ACCEPTING: audit_rounds.json records each
            round's {blocker,fix,nit}; when a non-passing round is CONVERGING (blocker 0,
            validation clean, round >= 2, 0 < fix <= auditAcceptMaxFix(2), fix not
            growing, fix budget left) the stage writes audit_accepted.json (the residual
            findings), routes to ONE final fixing round, and the re-entry passes
            agentlessly when validation is clean - so no KNOWN defect ships unfixed and a
            90-chapter book (where a fresh adversarial pass finds ~1 new small defect
            forever - a sampling process that never reaches zero) finishes instead of
            parking fix_loop_exhausted with round-N's findings still live. Blockers or a
            growing fix count still park, with the fix-count trajectory in the park
            message (StageResult.ParkMessage). contrib appends an acceptance note to the
            contribution rows (process metadata only - the public issue/PR payload is
            unchanged). runAgent also enforces agent.book_budget_usd (default 75) as a
            preflight: summed stage_runs cost (superseded rows included, so Retry can't
            duck it) >= budget parks ParkBudgetExceeded before spending more.
  contrib/  M7: everything GitHub-facing for contribution. TokenSource (secrets
            GitHubPAT first, else `gh auth token` - the token NEVER enters argv/logs/
            errors, leak-canary tested), a stdlib REST client (issues/gists/fork/
            contents/refs/pulls; injectable base URL; typed RateLimitError; APIError
            carries status + trimmed body only), composers that render the meta repo's
            issue-form markdown VERBATIM to metaissue's parser contract (headings +
            ticked checkbox items pinned from the form YAML; env-gated round-trip test
            AUDIOSILO_META_DIR runs the real `go run ./cmd/metaissue` -> verdict ok;
            >60000-byte bodies fall back to a secret gist link, which metaissue's
            attachment allowlist accepts), CoreProposal (+Validate: title/authors/
            language/narrators/sources required; an ASIN without a region is rejected,
            never silently dropped), Service (SubmitCore: per-book mutex, reuses an
            already-recorded core issue, persists the row BEFORE the park flip;
            SetWork validates the slug upstream), and the poller (jittered
            poll_minutes tick; issue rows advance submitted -> pr_open [FindIntakePR
            on branch intake/issue-<n>] -> merged/closed; a merged core PR's files
            name data/works/<shard>/<slug>/work.json -> SetBookWorkID [regardless of
            park state] -> Readmit [only when parked core_pending]; targeted
            ListBooksWithUnresolvedMergedCore query, no full scans; tokenless reads
            work). Imports neither scheduler nor api - reaches them via injected
            Readmit/Publish seams.
  auth/     single admin password (argon2id, generated + printed once on first run),
            opaque SHA-256-hashed session tokens, a per-IP login rate limiter; the
            Store interface is storage-agnostic (MemStore for tests; the SQLite
            store.AuthStore in production - the M0 JSON store was removed in M1)
  secrets/  named secrets (anthropic/openai keys, github PAT) in the OS keychain
            (go-keyring) with a 0600 secrets.json fallback; read API is presence-only
  store/    SQLite (modernc, pure Go; single writer + WAL) + append-only migrations:
            books, stage_runs, progress, events (durable log, 30-day prune), rates
            (per-stage EWMA unit rates - LIVE since M6), settings KV, sessions. M5's
            migration 0004 added stage_runs.{model,input_tokens,output_tokens,cost_usd} +
            AddOpenStageRunUsage (accumulates per agent invocation onto the open run) so
            per-stage cost rides on the book view. M6's 0005 added books.{chapters,
            park_code}; the park_code invariant (non-empty iff status is
            needs_attention) is enforced INSIDE SetBookState/SetBookStatus, not by
            caller discipline. M7's 0006 added the contributions table (one row per
            (book_id, kind characters|recaps|core); UNIQUE index makes
            UpsertContribution the crash-resume idempotency guard; status submitted|
            pr_open|merged|closed|local|already_covered) + books.narrators (JSON
            array like authors, feeds the core add-work proposal);
            ContributionSummary folds rows into the one aggregate chip status.
            Plain tested CRUD; AuthStore adapts it to auth.Store.
            Holds the SCHEDULING truth. The reliability round's 0008 added
            books.retry_at (RFC3339, '' = none; cleared with status, enforced like
            park_code) + stage_runs.superseded - Retry/readmit now SUPERSEDES success
            rows instead of deleting them, splitting the readers: SCHEDULING readers
            (CountStageSuccesses, SucceededStages*) filter superseded=0, MONEY readers
            (SumStageRunCost, StageRunTotals, ListStageRuns) include everything, so
            round counters reset on Retry but spend history survives.
  state/    per-book pipeline state machine: table-driven states/lanes/transitions,
            CanStart/NextState guards, the audit fix-loop cap. Pure, no I/O. M6 added
            ParkCode (typed park reasons - M7 added contrib_unavailable, core_needed,
            core_pending; the reliability round added budget_exceeded, so 14 now),
            MainlineNext (the optimistic mainline
            successor the ETA engine walks - the table's Next ordering is load-bearing:
            conditional/loop target first, mainline continuation LAST),
            ParseSeriesPos (moved here from scheduler so eta/scheduler share one
            parser), and IsParkedWith (the one status+park-code predicate api/contrib
            share instead of hand-rolling it).
  eta/      the PURE ETA engine (no I/O, no clock): per-stage unit kinds
            (chapter/chunk/book) + seed rates from the historical extraction metrics,
            EWMA Observe (alpha 0.3), book ETA = rate x remaining units over the
            optimistic mainline (loops are not predicted - documented), queue ETA = a
            greedy three-lane event simulation (LaneCaps injected from the scheduler,
            series locks via state.HoldsSeriesLock, retranscribe-first ASR ordering).
  scheduler/ one wake-on-event goroutine over three lanes (ASR cap 1 / agent cap =
            config, series-locked / mechanical cap 2) over an injected Executor +
            _done/<stage>.json sentinels (the CONTENT truth) and crash reconcile.
            Pause/resume/retry/cancel/delete + PurgeScratch (reclaim chapters/ when
            done/paused/failed). Publishes book.state/stage.progress/queue.stats.
            M2 runs the pipeline composite executor (real inspect/split, stubs beyond).
            M6: records EWMA rates from each stage's explicit StageResult.RateSample
            (units processed this run + productive seconds, measured by the stage AFTER
            setup and excluding agent rate-limit backoff - first-run tool/model
            downloads never contaminate learned rates; a nil sample records nothing),
            recomputes ETAs on every dispatch pass (idle-gated: no active books = no
            query/sim) and publishes deduped eta.update SSE (queue_seconds is null when
            idle); ETASnapshot/ETASeconds feed the books API. Progress reporting is
            display-only and resume-aware (a resumable stage's FIRST report is the
            already-complete baseline; skipped units never tick). M7: auto-purge -
            a book advancing to done reclaims its scratch in-line (the worker still
            holds the inflight slot; accounting runs under context.WithoutCancel so
            a shutdown-timed purge can't leave a stale gauge) and an async,
            WaitGroup-tracked startup GC purges done-with-scratch books after
            Reconcile (per-book reservation so a concurrent Delete sees busy); both
            gated by the autoPurge constructor param (from contribution.auto_purge).
            Reliability round: Retry's core was extracted into readmit(), shared by
            manual Retry, the contrib poller, and the new autoReadmitDue pass (each
            dispatch tick re-admits books parked agent_unavailable/agent_rate_limited
            whose retry_at is due, publishing a durable stage.note). readmit supersedes
            the CURRENT stage's successes ONLY for the round-cap park codes
            (qa_no_converge, fix_loop_exhausted - the latter also superseding fixing +
            wiping the audit trajectory files) so an availability park never destroys a
            loop's round history; parks with a plain code just clear status. Timed parks
            come from ParkError.RetryAfter (ParkWithCodeAfter): the rate-limit park uses
            the parsed reset + 2min (agent.ParseResetTime - epoch accepted only within
            (now, now+48h]; clock form host-local by documented assumption) floored at
            now+5min, else a 30min fallback; the mid-run transient NotAvailable park
            uses now+10min; the PREFLIGHT no-backend park carries NO retry_at (human
            only, so an unconfigured daemon parks once instead of churning). retry_at
            rides bookView and the book.state SSE frame (the web patch clears it when
            absent); pre-migration parks (retry_at '') never auto-readmit.
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
            NO business logic here. M6 added GET /books/{id}/sidecars (the preview
            envelope is composed in internal/pipeline.SidecarsView and INJECTED as a
            loader func - api never imports pipeline; 404 via ErrNoSidecars when no
            sidecar files exist) and GET /books/{id}/events (per-book durable log,
            limit clamped 1..500); bookView carries eta_seconds/started_at/park_code.
            M7 added GET /books/{id}/contrib/core (the prefilled proposal, 404
            absent), POST /books/{id}/contribute/core (409 unless parked core_needed,
            400 on Validate, 502 on rate limit), POST /books/{id}/work (manual slug
            set + readmit), GET /books/{id}/export (zip via injected
            pipeline.ExportArchive), bookView.contribution (aggregate chip) +
            bookDetail.contributions (rows), and the contrib.update SSE event; all
            new endpoints have allowed + denied auth tests.
  web/      go:embed of the SPA (build-tag selected) + SPA-fallback static serving
  server/   http.Server wiring, graceful shutdown, the startup banner
web/          the SPA: Vite + React 19 + TS + Tailwind v4 (npm, Node 24); dist/ is embedded
              src/lib/ holds pure, vitest-tested logic (apiClient, candidates, books,
              pipelineState, recentRoots, useEventStream, scanStore; M6 added timeline,
              duration, bookLog, parkReasons, doneBoard, time, useLazyDetail, and
              expressive.ts - VENDORED from audiosilo-meta site/src/lib/expressive.ts
              with its tests, keep it tracking upstream; M7 added coreProposal,
              contributionSettings, formNumbers, throttleTrailing, download);
              src/components/ui/ holds the shared Modal + Field primitives
              (extracted in M7 - new modals/forms use them, don't re-inline the
              chrome); src/components/
              {library,running,done}/ are the Library/Running/Done tab views; components
              stay thin over src/lib. The timeline's stage graph is a hand-mirror of the
              Go state table - a drift-guard test pins its stage set to the label maps. The Library tab's scan + selection state lives in
              scanStore.ts - a module-level external store (useSyncExternalStore)
              owning the 700ms poll loop, so tab switches (AppShell unmounts
              panels) and reloads (GET /scans reattach) never lose a running scan;
              sign-out calls scanStore.reset(). API calls key books by the
              daemon-computed absolute source_path (NEVER a client-side join);
              the relative path is display/selection only.
scripts/build-web.sh   build the SPA + embed it into bin/ (-tags embedui)
Dockerfile             multi-stage: node build -> go build (embedui) -> two runtime
                       targets from the SAME shared stages: `runtime` (debian-slim,
                       CPU, the default) and `runtime-cuda` (nvidia/cuda, GPU - the
                       CUDA whisper-cli is toolfetched at runtime, not baked in).
                       Both apt-install ffmpeg (so toolfetch never downloads it) and
                       run non-root with a chown'd /data. image.yml builds both via
                       `--target`.
.goreleaser.yml        M8: native-binary release config (draft, embedui, archives+checksums)
```

**Dependency direction** (transport-only rule): `server -> {api, auth, secrets,
events, config, store, scheduler, metaops, pipeline, web}`; `api -> {auth, secrets,
events, config, store, scheduler, metaops}`; `scheduler -> {store, state, eta, events}`; `eta -> state` (pure, imported BY
scheduler - never the reverse);
`pipeline -> {audio, asr, transcript, qa, spelling, agent, repair, toolfetch, scratch,
secrets, fsutil, store, state, scheduler}`; `agent`/`repair` are leaf helpers (no
scheduler/store deps; `repair -> qa` for the shared Python-compat gram/repr helpers);
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
  and retranscribe-first ASR ordering). Rate observation is stage-owned: each stage
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
  hazard), rewrites the files' `work` field + appends the
  {type:"community",ref:"audiosilo-sidecars"} provenance source + re-canonicalizes
  in place, skips upstream-covered dimensions (already_covered rows), then submits
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
    over title/authors/series/narrators/asin/isbn) and **series-order sort**
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
    gauge that does not bump `updated_at`) and a **bucketed order** - active/running
    book(s) on top (furthest-along the mainline first), then the queue FIFO, then
    paused/needs_attention/failed, then done (`web/src/lib/books.ts` `sortBooks`).
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
  fix count (<= 2) at round >= 2 writes `audit_accepted.json`, takes ONE final fix
  round, and passes agentlessly (no known defect ever ships unfixed; residuals
  recorded + surfaced on the contribution note); Retry on `fix_loop_exhausted`
  grants a genuinely fresh loop (fixing count + trajectory reset - previously it
  re-parked after one wasted audit); (4) **availability self-resume** -
  `books.retry_at` (migration 0008) + a scheduler auto-readmit pass for
  `agent_unavailable`/`agent_rate_limited`, reset-time parsing with a 5min floor,
  transient-NotAvailable in-process retries, and a human-only preflight park when
  no backend is configured; (5) **cost containment** - `agent.book_budget_usd`
  (default 75) parks `budget_exceeded` before more spend, and Retry SUPERSEDES
  stage_runs instead of deleting them so spend history (and the budget) survive.
  Deferred, documented: fact_pass output-token cost (~$18 on 90ch; the facts are
  dense content, not boilerplate), auditing stays on opus (it is the public-
  contribution quality gate; acceptance bounds its rounds), and the audit model's
  per-round history lives in work-dir JSON, not the DB.

Still **not built**: signed installers / a friendlier packaged client (a possible
follow-up per the meta EXTRACTION roadmap); a separately-deployable UI-only image
(scoped out of M8, deferred). The contributing stage submits sidecars but never
retracts them - cancelling a core_pending book leaves its already-opened add-work
issue for a maintainer to close. **Ebook input** (accept an EPUB and skip the
audio / ASR / QA / spelling stages, reusing the fact_pass -> synthesis -> audit
back half) is designed but unbuilt - see [EBOOK-INPUT.md](EBOOK-INPUT.md). All
four tabs report `ready` on `/system`. Keep this file honest as milestones land.
