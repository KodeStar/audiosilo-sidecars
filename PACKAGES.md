# PACKAGES.md - AudioSilo Sidecars per-package internals

The full annotated package map: per-package invariants, incident history, and
load-bearing rules, moved verbatim from CLAUDE.md's "Package layout" section so
the always-loaded guide stays lean. **Read the entry for any package you touch
before changing it.** CLAUDE.md keeps the compact tree and the dependency
direction; this file is the detail.


```
cmd/audiosilo-sidecars/   entrypoint: `serve` (default) + `version`; flags --data, --listen
cmd/audiosilo-bench/      private corpus preparation, isolated model-matrix runs,
                          aggregate reports, and snapshot-only historical telemetry
internal/
  config/   config.yaml in <data>/ + AUDIOSILO_SIDECARS_* env overrides; Load/Save/Validate.
            M1 added library_roots (scan allow-list), metadata.base_url;
            agent.queue_concurrency controls agent-lane books and
            agent.max_agents_per_book controls safe within-book fan-out; legacy
            agent.concurrency retains its historical global cap. M2 added tools.
            {ffmpeg_path,ffprobe_path,auto_download} - the SINGLE source of truth for
            tool paths (the ffprobe knob lives under tools.*; the folder scan uses
            the resolved path). M3a made asr.* live: backend
            (auto|mlx-whisper|whisper-cpp), model, language, whisper_cli_path. There
            is no asr.device knob (no backend honors an override yet; /system reports
            the DETECTED device). Changing asr.backend or the tool paths takes effect
            only on a daemon RESTART (the backend is resolved once at startup, unlike
            cors_origins, which the API re-reads live per request). M5 made agent.*
            LIVE: backend (""|claude|codex), capacity, claude_path/codex_path,
            timeout_minutes, and the per-stage claude/openai model maps (keys are agent
            stage names). Validate rejects an unknown backend, an unknown model-map key,
            or timeout_minutes < 1; Default() seeds the claude map. M7 added
            contribution.{mode [issue|local], core_repo (add-work), community_repo
            (sidecars), auto_purge, poll_minutes, api_base_url} (restart-to-apply;
            api_base_url for tests/GHE). Retired settings load via
            foldLegacyContribution + Config.Deprecations: `repo` -> core_repo (an
            explicit core_repo wins), mode `pr` -> issue. The reliability round added
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
            per-chapter progress, ctx-cancel clean). A marker-style split first
            STAGES the source m4b into the work-dir split-source/ directory
            (stageMarkerSource: one sequential 4MB-buffered copy, atomic rename, a
            complete staged copy is reused by a retry only while its size still
            matches a statable source - an unreachable source trusts the cache, a
            REPLACED source re-stages - skipped entirely when every chapter is
            already split, removed after success; the DIRECTORY is registered in
            scratch's artifact table so a failed/cancelled split's copy is
            reclaimable; a staging failure DEGRADES to splitting directly from the
            source - staging is an optimization, never a precondition) - ffmpeg
            otherwise reopens the same often-SMB-mounted file once per chapter, and
            enough rapid random-access opens can wedge macOS's SMB client. Both
            halves are TIME-BOUNDED so a wedged mount cannot hang invisibly behind
            the liveness heartbeat: each chapter conversion gets max(10min, 2x its
            audio duration) and the staging copy max(15min, a size-derived budget) -
            a bound hit is a loud, non-transient error, mirroring
            asrChapterDecodeBound. IsTransientSourceErr owns the transient
            FUSE/Nextcloud EINTR shape the pipeline's split retry classifies with.
            Pure/tool-driven, no scheduler deps.
            chapterFromMarker's vocabulary covers "Chapter N", with the title introduced
            by punctuation, an underscore, or NOTHING BUT WHITESPACE ("Chapter 1
            Suffering from Success", "Chapter 1 (Series Name)", "Chapter_1" - the literal
            word "Chapter" is what makes a loose tail unambiguous), digits or
            spelled-out, plus "N. Title" and bare-number tables ("001".."064", "-1-").
            The tail still REQUIRES a separator, which is what keeps "Chapter 10a"
            unrecognized: a split chapter announces one number twice and no contiguous
            manifest can express that, so those books belong with the agent.
            ROUTING IS MarkerStats.Usable() = Contiguous AND Complete, and both halves
            are load-bearing. Contiguous is the numbering check. Complete is COVERAGE:
            UnmappedSpans reports every run of recording time no chapter covers, with the
            marker titles occupying it, and an interior run >= MaxInteriorGapSec (60s) or
            an edge run >= MaxEdgeGapSec (180s) means narration was lost. Numbering alone
            said nothing about it, so 27 books silently dropped 61 hours of Interludes,
            Side Stories, Prologues and Epilogues - never split, never transcribed, no
            park and no note, their sidecars written as though that audio did not exist.
            The two thresholds are measured, not guessed: over the 294-book corpus
            interior holes are bimodal (one 9s encoder artifact, then nothing until 211s,
            above which all 173 are real narration), while the edges are where credits,
            bloopers and retailer samples legitimately live. A title-only marker table
            (no marker states any number) that TILES the recording is numbered by its own
            time order - positionalChapters, MarkerStats.Positional - exactly as a
            multi-file book is numbered by file order; refusing only for the single-file
            case was an inconsistency, not a safety property. A GAPPY title-only table is
            still not numbered: the hole is precisely what must reach a human.
            ReparseMarkerManifest recovers a draft for free after a parser upgrade (gated
            on the draft being non-contiguous, NOT on coverage - an agent-harvested map
            legitimately has edge holes where it excluded credits, so widening the gate
            would re-derive over the agent's work; it preserves the draft's Title AND
            Duration, since the stage bounds the agent's intervals against that duration).
            Inspect/Reparse return MarkerStats {Seen,Recognized,Contiguous,Positional,
            Unmapped,Complete}: Seen>0 with Recognized==0 is a parser vocabulary gap, NOT
            a markerless file - the two were indistinguishable in the metrics for three
            dialects running. Recognized stays the honest PARSER count, so a positionally
            numbered table reports Recognized 0 and Positional true rather than claiming a
            dialect was understood. A StyleFiles book reports Seen/Recognized 0 (its
            probe.json holds no marker table at all) and Complete true (its chapters are
            laid end to end from each file's duration), so markers_seen never fabricates a
            dialect signal. The deterministic reparse records NO RateSample - it is a
            sub-millisecond re-derivation of a stage whose real cost is the agent round
            it skipped, so observing it would collapse the markers_normalizing EWMA (the
            same rule as repair's free known-failed skip).
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
  ebook/    M9: the epub front half. BuildUniverse turns audiosilo-meta pkg/extract's
            per-section split into the LOGICAL chapter universe the sidecars publish
            positions against, then WriteChapterText/WriteManifest materialize it.
            Two rules are load-bearing: quarantine is POSITIONAL (everything outside
            the first..last numbered section is excluded regardless of length -
            a trailing promo excerpt of ANOTHER book is a full chapter, so any
            word-count rule waves it through), and a Loose label reading is accepted
            only when the whole book's labels form a contiguous run (Contiguous),
            because "I An Irate Neighbor" and "MIX" are ambiguous per-label. Find
            walks a library for .epub files and reads each OPF for identity, never a
            content document. Pure + table-tested; a 33-book corpus test is env-gated
            on AUDIOSILO_EPUB_DIR.
  scratch/  per-book DirSize gauge + Purge (removes chapters/ and split-source/,
            keeps durables), confined to the work root. Reclaimed manually (purge-scratch), by M7's
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
            reference.go is the OUTSIDE-EVIDENCE pre-pass (BuildReferenceMatches ->
            spelling_reference_matches.json): it edit-distance-matches each candidate
            against reference vocabularies and reports transcript forms no reference
            spells that way. It exists because the agent's only native signal is
            intra-transcript disagreement, which a CONSISTENTLY misheard name never
            produces - "Torrin" 369 times and "Toren" never reads as settled, so a
            whole Wandering Inn series published Torrin/Floss/Terriarch/Goddard.
            ReferenceSource.Authority is the load-bearing part: VERIFIED (the meta
            series glossary, the publisher's marker titles) comes from outside the
            recording and may contradict it; CARRYOVER (the predecessor's ledger)
            may not. Only a verified source marks a form "known" (and a known form
            is never proposed against) - letting carryover settle a form would make
            a propagated error immunise itself, which is exactly how one mistake
            travels down a series. Bounds (maxMatchDistance 2, also <= len/3, first
            letter must match) are calibrated against the six real misspellings;
            NamesFromTitles mines marker titles ("Chapter 9: Toren") and yields
            nothing for the bare-number tables real volumes often ship.
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
            next host-local occurrence). Post-M9 round: isRateLimit no longer
            treats a bare "429" as a rate limit - RFC3339 fractional seconds and
            usage counters contain those digits (".294295Z",
            "output_tokens":429), so a timestamped stderr line misclassified a
            validation failure; the status code now requires nearby protocol/error
            context, wide enough for the JSON and status_code=429 shapes the old
            substring caught. runCLI bounds Cmd.Wait with a 5s WaitDelay
            (cliPipeWaitDelay) so a detached CLI helper holding the inherited
            stdout/stderr pipe cannot hang a finished stage; ErrWaitDelay after a
            successful exit is treated as success. SleepCtx is the shared
            ctx-aware backoff sleep (pipeline's split retry reuses it).
  benchmark/ private, provider-neutral post-ASR evaluation: allow-listed corpus
            preparation with input digests; fresh per-run work trees and databases;
            stage-specific model/effort routes; the real fact/synthesis/audit/fix
            pipeline; independent holdout audits; hard-gated aggregate/Pareto reports;
            accepted-reference judge calibration; and read-only historical telemetry
            over a caller-supplied DB snapshot.
            Transcript corpora and generated results must remain outside the repo.
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
            (0 = derive as before, byte-identical). Clip windows are CHAPTER-RELATIVE
            seconds and bounded by qa.ClipStartInRange (the shared floor predicate,
            qa.ClipStartFloorSec): the plan validator rejects an out-of-range value at
            authoring time with retry feedback, the retranscribe dispatch skips it as
            clips_unlocatable BEFORE any cut or destructive prep (with an
            action-appropriate "out of range" note), and the repair layer is defense in
            depth - the located path IGNORES an out-of-range override (keeps the derived
            start), the directed and MID paths no-op. This closed the live incident where
            an absolute source-file timestamp cut a negative-length clip and hard-failed
            the book forever. The cut clip file is keyed on
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
            ClipResult.Unlocatable() reports a no-op: the true tail no-op (no loop AND no
            override), a MID window whose start is out of range for the chapter, or a
            dispatch-level out-of-range rejection - all bucketed as clips_unlocatable +
            a note (naming the missing window, or the out-of-range value and the
            chapter-relative rule) so the adjudicator can correct the plan next round.
  pipeline/ composite scheduler.Executor: routes inspecting -> audio.Inspect,
            splitting -> audio.Split, asr -> the per-chapter internal/asr loop
            (resumable: skip complete raws, delete+retry malformed, freeze each raw
            0444, write asr.json provenance, account scratch; each chapter decode is
            bounded by asrChapterDecodeBound = max(10min, 3x audio duration) - a
            whisper repetition-collapse decode that blows past it is killed and retried
            ONCE with NoContext, and a second timeout parks asr_decode_timeout rather
            than running for hours; the ASR/retranscribe heartbeat ticker advances BOTH
            heartbeat_at AND progress_at, since those stages have no mid-chapter output,
            so a live-but-slow decode trips neither the supervisor's stale-heartbeat nor
            its no_progress detector), sanitizing ->
            internal/transcript normalization, qa_sweep -> the internal/qa sweep
            (writes both reports, branches on Report.Clean()). M5 made EVERY remaining
            stage real: markers_normalizing/qa_adjudicating/spelling_research/fact_pass/
            synthesizing/auditing/fixing are AGENT stages (staged dir + rendered prompt
            + validated outputs via the shared runAgent driver, usage recorded onto the
            open stage_run after every invocation), while retranscribing/correcting/
            validating are MECHANICAL (ASR+repair / spelling engine / canonicalize+ngram).
            validating skips the n-gram scan only for a sidecar NGram would refuse
            (ngramGate).
            M7 made contributing real (contrib_stage.go: slug reconcile -> skip-if-
            covered -> submit per contribution.mode, resume-idempotent via the
            contributions rows; export.go composes the download zip + core-proposal
            JSON injected into api) - EVERY stage is now real. Sidecars go to
            Config.ContribCommunityRepo (issue mode) or <export>/<slug>/<kind>.json
            (local mode, also the zip layout). resolveWorkSlug adopts a 301 survivor
            (contrib.AdoptLiveWork) and parks core_pending with contrib.ReleaseWaitMsg
            while a merged core row's work is unreleased; a duplicate-answered core
            row parks core_needed with CoreDuplicateMsg.
            spelling_research additionally assembles OUTSIDE spelling evidence before
            it stages anything: referenceSources() ranks the metaops series glossary
            and the publisher's marker titles as VERIFIED against the predecessor's
            ledger as CARRYOVER, and BuildReferenceMatches turns that into the staged
            spelling_reference_matches.json. The metaops dependency is reached through
            the OPTIONAL MetaGlossary interface (type-asserted off the existing
            e.meta), so a nil/!ok client simply runs the stage exactly as before -
            the glossary is evidence, never a precondition.
            markers_normalizing's manifest contract has a COVERAGE half alongside the
            numbering one: validateMarkersManifest checks the corrected map against the
            RAW marker table from probe.json (never the draft - the draft is exactly what
            may have dropped a marker) and rejects any unmapped span the verdict did not
            DECLARE in its `excluded` list (title/start/end/reason, surfaced as a stage
            note). Nine books had interludes dropped by an agent that was consulted and
            answered confidently, because numbering was all it was checked against.
            Declaration rather than prohibition is deliberate: an omnibus file carrying a
            preview or the whole NEXT book genuinely must leave audio out, so the rule is
            that a drop must be stated, not that it is forbidden. markers.md correspondingly
            tells the agent that an unnumbered narrative section (Interlude/Side Story/
            Intermission/Prologue/Epilogue/"Chapter 10a") IS a chapter, that including it
            shifts nothing (positions come from the narration later), and to INCLUDE when
            in doubt. The load-bearing
            invariants live in
            the staging (synthesizing/auditing dirs hold NO transcripts, independent
            fact_pass chunk and QA partition dirs hold only their own chapter range and spoiler-bounded
            spelling sheet, and a separate notes-only assembly writes the compact final
            knowledge sheet). LOGICAL CHAPTER UNIVERSE (edgechapters.go): the manifest
            counts every audio FILE as a chapter, but an Audible "This is Audible"
            intro or publisher-credits outro file is not a story chapter - counting
            them deadlocked the audit loop on two real books (the auditor demanded
            recaps through phantom chapters the fixing stage could never satisfy,
            burning $200+). classifyBookEdges (content-driven, any manifest style)
            probes the first/last 8 chapters' transcript word counts (< 120 words AND
            < 180s duration at the edges only; probe-saturation and all-small books
            degrade to no exclusions) and derives the LOGICAL story-chapter count +
            stage notes. It reads each chapter's OPENING for EVERY chapter, not just the
            edges (256 bytes, unlike the whole-transcript word count) - the numbering
            below is only as good as its coverage, and reading openings at the edges
            alone left a long book's interior with no evidence, so any unnumbered
            section in the middle had to be guessed around.
            Synthesis/audit/audit-verify/fix prompts and the mechanical
            validateSidecars position cap use the logical count (sidecarStageInputs
            deliberately does not expose the raw manifest); the fact-pass CHUNK keeps
            file-numbered `## Chapter N` headings (matching staged transcripts + the
            chunk validator - never renumber), and the ASSEMBLE step is the ONE
            file->spoken renumbering boundary (its note renders the concrete offset
            mapping). The offset is DERIVED FROM THE NARRATION, never from counting
            files: deriveNarratedNumbering reads the chapter number each probed file
            ANNOUNCES (audio.SpokenChapterNumber - digits, spelled-out, optional
            "Chapter" prefix; the whole candidate must parse as one number, so prose
            opening "One more time" is rejected) and takes the offset from a run of
            >= minAnnouncementRun (3) CONSECUTIVE files announcing CONSECUTIVE numbers.
            Both conditions matter: one stray parse cannot shift a book, and files
            repeating the same number (a stuck transcript) are not a numbering. That run
            is the ANCHOR - the only announcements trusted outright; the derivation then
            extends outward, believing a further announcement only while it stays
            monotone, so a stray parse outside the run is discarded rather than allowed to
            renumber the book. Files
            before the first numbered chapter are FrontMatter (the meta schema's
            position 0 - an unnumbered Prologue is NOT chapter 1) and files past the
            last numbered one are EndMatter (an Epilogue/bloopers reel: outside the
            numbered range, its material belongs in the whole-book summaries, never a
            chapter-gated entry). With no agreeing run the classifier keeps the old
            positional behaviour byte-identically.
            The result is a PER-FILE map (edgeClassification.Positions, read via
            PositionOf), not one offset, because a single offset cannot describe a book
            with unnumbered material in the MIDDLE - an Interlude between chapters 9 and
            10, or a chapter split across "10a"/"10b". Such a file takes the number of the
            chapter it PRECEDES: rounding UP is the direction that cannot leak, since
            finishing chapter 10 implies having heard the interlude before it while
            finishing 9 does not. Three cases must not be confused when filling the gaps:
            a file that was read and announces nothing is unnumbered narrative (round up);
            one that announced a number which was NOT believed is still a numbered chapter
            (keep the shift); and one with no transcript at all is too (silence is not
            evidence). Folding the last two in with the first collapsed a 59-chapter book
            onto chapter 55. ConstantOffset() reports whether the map is a single shift,
            so composeAssembleNote keeps its byte-identical "subtract N" sentence for the
            common case and renders an explicit file->chapter run table only when it is
            not - inventing a formula for a book that has none is the original error. This was a real incident: a 64-file
            book (Audible intro, unnumbered Prologue, chapters 1-59, Epilogue,
            bloopers, credits) counted as 62 logical chapters at offset 1, so EVERY
            reveal and recap gate sat a chapter early and the position cap allowed
            gates past the end of the book; the auditor correctly rejected them as
            disclosing later material and the audit/fix loop burned round after round
            chasing one clause at a time. Note the numbering is baked into
            knowledge-final.md, so a book affected by this must be re-run from
            fact_pass - retrying the fix loop only chases symptoms. Exclusions and the
            derived mapping always surface as a stage note. Fact chunks and QA
            chapter partitions run concurrently
            behind the same executor-wide invocation semaphore. New capacity uses
            queue_concurrency * max_agents_per_book; legacy concurrency remains a
            non-multiplied global cap. Spelling reference_files are restricted to the
            daemon-staged carryover. Constructed in server.go with the
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
            with a position>=95% fallback when the hit has no usable time). An UNTIMED
            cross-segment hit (no first_sec, written as pos -1) is the characteristic shape
            of a tail loop re-reported off the untouched transcripts-json/ layer, so that
            positional fallback can never fire for it; crossHitTailCovered therefore falls
            back to a phraseResolver (repairedPhraseResolver, injected so the rule stays
            unit-testable) asking the decisive question the window arithmetic cannot: is the
            looped phrase still in transcripts-repaired/? Gone = the splice resolved it;
            still present = the repair under-covered and the chapter stays with the agent.
            Without this EVERY untimed residual went to the agent, and adjudicate.md tells
            the agent NOT to disposition a chapter a prior tail_clip round already repaired -
            so it omitted one ("intentionally omitted" in its notes), the plan validator
            rejected the plan for a missing required entry, and the stage failed identically
            on every retry until the book parked agent_validation_exhausted. adjudicate.md
            now also states that the auto-accepted list is EXHAUSTIVE - any other flagged
            chapter the agent believes is already repaired still needs an explicit accept,
            because an omission is not a disposition. A MID-CHAPTER
            multi-loop is covered ONLY by a recorded MID window (never a tail window); a mid
            window with an untimed hit is conservatively NOT covered; (3) plan
            clip_start_sec/clip_end_sec (per tail_clip/mid_clip entry) feeds the repair
            known-failed skip above, and (*qa.Plan).Validate(rep, durations) bounds every
            clip window against its chapter's manifest duration (chapter-relative
            seconds; an at-or-past-duration value is rejected with retry feedback naming
            the chapter, value, and duration - adjudicate.md states the timebase). wph outliers, within-segment hits, non-end-fade runs, a
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
            validation clean, round >= 2, 0 < fix <= auditAcceptMaxFix(2), actionable
            findings not growing (so BLOCKER -> FIX is progress), fix budget left) the
            stage writes audit_accepted.json (the residual
            findings), routes to ONE final fixing round, and the re-entry runs a focused
            semantic verifier over those exact accepted findings when validation is clean -
            so a mechanically valid but unapplied FIX cannot ship and a
            90-chapter book (where a fresh adversarial pass finds ~1 new small defect
            forever - a sampling process that never reaches zero) finishes instead of
            parking fix_loop_exhausted with round-N's findings still live. Blockers or a
            growing fix count still park, with the fix-count trajectory in the park
            message (StageResult.ParkMessage). Both audit.json writers (audit.md AND
            audit_verify.md) feed the same DisallowUnknownFields reader, so EACH must
            state the exact two-field (`pass`/`findings`) shape and name the `nit` key
            agents reach for - a prompts drift-guard test (auditJSONPrompts) pins that.
            audit_verify.md had drifted to "the normal audit shape" while inviting NIT
            reporting, so a PASSING verify emitted {"pass":true,"nit":0,"findings":[]}
            and the reader rejected it on every retry: a finished book parked
            agent_validation_exhausted one cosmetic key from done. A nit is a findings
            entry with severity NIT, never a top-level key.
            contrib appends an acceptance note to the
            contribution rows (process metadata only - the public issue/PR payload is
            unchanged). runAgent also enforces agent.book_budget_usd (default 75) as a
            preflight: summed stage_runs cost (superseded rows included, so Retry can't
            duck it) >= budget parks ParkBudgetExceeded before spending more.
            Post-M9 round: the split stage retries a transient
            audio.IsTransientSourceErr in-place (3 attempts, resumable - completed
            chapter FLACs are skipped; agent.SleepCtx backoff) instead of failing
            the book, and runs under the shared stage heartbeat
            (runWithStageHeartbeat, the generalization transcribeWithHeartbeat now
            wraps) so the supervisor cannot kill a slow single-chapter ffmpeg
            conversion between progress reports; the split RateSample starts at the
            first progress report (audio.Split emits it after staging) and
            subtracts the backoff slept, keeping the staging copy and retry sleeps
            out of the learned EWMA. validateMarkersManifest's coverage check
            accepts adjacent per-marker exclusion declarations whose gap-free union
            covers one coalesced unmapped span (exclusionsCover, same 1s tolerance,
            never bridging a real undeclared gap) - UnmappedSpans coalesces
            adjacent markers while the verdict schema asks for per-marker
            declarations, so requiring one coarse declaration rejected the more
            precise correct verdict.
  contrib/  M7: everything GitHub-facing for contribution. TokenSource (secrets
            GitHubPAT first, else `gh auth token` - the token NEVER enters argv/logs/
            errors, leak-canary tested), a stdlib REST client (injectable base URL; typed RateLimitError; APIError
            carries status + trimmed body only), composers that render the meta repo's
            issue-form markdown VERBATIM to metaissue's parser contract (headings +
            ticked checkbox items pinned from the form YAML; env-gated round-trip test
            AUDIOSILO_META_DIR runs the real `go run ./cmd/metaissue` -> verdict ok;
            >60000-byte bodies fall back to a secret gist link, which metaissue's
            attachment allowlist accepts), CoreProposal (+Validate: title/authors/
            language/narrators/sources required; an ASIN without a region is rejected,
            never silently dropped), Service (SubmitCore: per-book mutex, reuses an
            already-recorded core issue, persists the row BEFORE the park flip;
            SetWork validates the slug upstream), and the poller. Where things live:
              poller.go  row lifecycle (FindIntakePR -> pr_open -> merged/closed),
                         intake verdicts as the note's last segment (re-checked at
                         most hourly; comments re-read only when the issue changed,
                         newest bot comment wins), learnCreatedWork (base.sha...head.sha compare,
                         works-pack ENTRY KEYS at merge base vs head), the release
                         gate (admitWhenLive / releaseCorePending)
              notes.go   JoinNotes, AdoptLiveWork (shared with the stage)
              github.go  the REST client: issues, gists, pulls, compare, contents
            ServiceDeps.CoreRepo is where SubmitCore opens add-work issues; every
            other call follows the repo a row records.
            Imports neither scheduler nor api - reaches them via injected
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
            round counters reset on Retry but spend history survives. The Library
            New-view round's 0012 added library_sightings (source_path PRIMARY KEY,
            first_seen_at, last_seen_at, acknowledged_at, baseline) - path-keyed
            like candidate_overrides with no FK to the book index, so a sighting
            survives an enqueue, a delete and a rescan. RecordSightings is
            insert-or-bump: a known path keeps its first_seen_at AND its baseline
            flag (a rescan must never reset when a folder appeared, nor promote a
            pre-existing book into the New view) and only bumps last_seen_at;
            AcknowledgeSightings ignores unknown paths, and HasSightings answers
            the baseline question ("is this the first batch?") without reading the
            table - that pair IS metaops.SightingRecorder, which the DB satisfies
            directly (no adapter). Its timestamps are plain
            RFC3339 seconds (SightingLayout), NOT the store's fixed-width
            nanosecond layout, because first_seen_at is served verbatim on the
            wire; all three columns share it, so compares stay chronological.
  state/    per-book pipeline state machine: table-driven states/lanes/transitions,
            CanStart/NextState guards, the audit fix-loop cap. Pure, no I/O. M6 added
            ParkCode (typed park reasons - M7 added contrib_unavailable, core_needed,
            core_pending; the reliability round added budget_exceeded; the ASR
            decode-timeout hotfix added asr_decode_timeout, and M9 added
            ebook_unreadable / ebook_no_chapters / ebook_chapters_not_confident,
            so 20 now),
            MainlineNext (the optimistic mainline
            successor the ETA engine walks - the table's Next ordering is load-bearing:
            conditional/loop target first, mainline continuation LAST),
            ParseSeriesPos + SeriesBreadthRanks (shared scheduler/ETA series ordering),
            and IsParkedWith (the one status+park-code predicate api/contrib share
            instead of hand-rolling it). The post-M9 round added the SourceIO
            Def column + ReadsSource (the stages that open the original library
            item; the scheduler's mechanical-lane serialization and the ETA
            simulation both consume it).
  eta/      the PURE ETA engine (no I/O, no clock): per-stage unit kinds
            (chapter/chunk/book) + seed rates from the historical extraction metrics,
            EWMA Observe (alpha 0.3), book ETA = rate x remaining units over the
            optimistic mainline (loops are not predicted - documented), queue ETA = a
            greedy three-lane event simulation (LaneCaps injected from the scheduler,
            series locks via state.HoldsSeriesLock, retranscribe-first then
            breadth-first-across-series ASR ordering). The post-M9 round added
            LaneCaps.MechanicalSourceIO: the simulation charges a mechanical stage
            with state.ReadsSource against that scarce sub-slot (continue, never
            break - a work-dir-only stage still overlaps a library read), so the
            queue ETA models the scheduler's source-IO serialization instead of
            predicting 2x the real inspect/split parallelism.
  scheduler/ one wake-on-event goroutine over three lanes (ASR cap 1 / agent cap =
            config, series-locked / mechanical cap 2) over an injected Executor +
            _done/<stage>.json sentinels (the CONTENT truth) and crash reconcile.
            The ASR lane is ONE serial MLX slot shared by full-book and corrective
            transcription - the post-M9 round REMOVED the separate corrective slot
            (overlapping decodes pushed the second process into the OOM killer on
            unified-memory Macs); corrective work takes priority when the slot
            frees. The mechanical lane additionally serializes SOURCE-READING
            stages (inspect/split/extract, the state.Def SourceIO column read via
            state.ReadsSource) to sourceIOCapacity 1 - concurrent random access
            through one SMB share can wedge macOS's SMB client - while
            work-dir-only stages keep the second slot.
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
  supervisor/ the health-tick babysitter (config supervisor.*): classifies incidents
            over book/stage-run snapshots (deterministic classifiers + optional
            model-assisted lane), decides bounded recovery actions, and applies them
            through the scheduler seam. Three invariants are load-bearing (all fixed
            after live park/readmit ping-pongs): (1) the parked-recovery
            FINGERPRINT is stationary - derived from park code + stage + the latest
            non-superseded stage-run error, never from book.Error, which every
            park_escalate rewrites (state.SupervisorMessagePrefix prose is stripped);
            (2) a durable cross-Kind AUTO-RECOVERY CAP: at most MaxAttempts automatic
            retry/readmit/supersede_rerun/requeue/fallback_backend actions per book
            per rolling 24h (store.CountAutoRecoveriesSince; terminate_requeue is
            deliberately excluded as restart hygiene), enforced on BOTH the
            deterministic and the model-assisted lanes - past the cap the decision
            converts to park_escalate + approval_required (human review); (3) the
            AttemptGrowthFactor checks compare against a stage's HIGH-WATER MARK over
            all its successful attempts (priorSuccessBaseline - max duration/tokens/
            cost, cost only across attempts of the same comparableCost kind), never
            whichever attempt happened to finish last, AND the duration check stays
            silent below the absolute minGrowthElapsed (20min) floor. An agent stage's
            duration tracks the work it was handed - a fixing round carrying three
            blockers legitimately runs several times longer than one carrying a small
            edit - so a loop stage's recent short round is an arbitrary sample, not a
            budget. Without both guards a healthy 3m13s fixing run was stopped
            (stop_budget) against a 62s predecessor at 1.8% of MaxStageDuration, and
            the resulting park then spent the book's whole auto-recovery budget.
            Post-M9 round: a dead recorded process is classified missing only past
            processExitGrace (90s - it is measured against a stage-run heartbeat
            refreshed on a 60s cadence, so the grace must exceed that cadence plus
            the runner's 5s pipe-wait; a 15s draft failed for ~75% of child exits)
            from the freshest heartbeat/start - the child's exit and the runner's
            durable process_active clear are sampled independently, and the gap
            between them read as a disappeared worker on a successfully completing
            stage; and collectArtifactStatuses trusts an OPEN stage run over the
            stale book snapshot (the scheduler can advance the state between the
            two reads), so an intentionally absent sentinel for an in-flight rerun
            is not reported broken.
  metaops/  meta.audiosilo.app client (coverage/lookup, capped 1h TTL caches,
            graceful degrade) + async folder-scan job manager over audiosilo-meta
            pkg/scan + the library_roots PathAllowed check. glossary.go adds
            SeriesGlossary (work -> its series -> the OTHER volumes' contributed
            character names/aliases, <= 12 siblings, own TTL caches), the canonical
            spelling evidence the spelling stage checks a transcript against; it
            EXCLUDES the book's own sidecar (a book checked against itself makes its
            mistakes self-attesting) and drops descriptive placeholder labels ("The
            missing young gnoll child") since those are prose, not spellings. Every
            no-data path (disabled client, no series, outage, uncontributed
            siblings) returns an empty glossary and a NIL error - a metadata outage
            must never park a book. Coverage resolves
            asin -> isbn -> a fuzzy title-search fallback scored by
            audiosilo-server's pure-stdlib pkg/match (Coverage carries matched_by
            "asin"|"isbn"|"search"|"manual" + work_title provenance). The search
            fallback walks the ladder.go RETRIEVAL LADDER: over a real 1147-book
            library the dominant failure was retrieval, not scoring - a decorated
            shelf title ("Supermage : Rise To Omniscience, Book 1", "Halo:
            Primordium (Unabridged)", "Artemis Fowl 4 - The Opal Deception")
            retrieves NOTHING even when the work is indexed under a clean title. The
            ladder is 9 ordered, de-duplicated query shapes (raw title, punctuation-
            normalized, CleanTitle, pre-subtitle, post-separator tail, bare trailing
            volume stripped, title+author, folder leaf, parent folder), the raw title
            FIRST and ALWAYS (the minimum query length is a floor on the DERIVED
            rungs only - a book called "It" must still search something) and an early
            exit at the first rung match.Best accepts, so a book that already
            resolved still costs one request. Each rung carries its own matchTitle,
            and the de-duplication key is the WHOLE step: rung 5 and rung 8 can send
            the same query scored against different titles, and dropping the second
            deletes the leaf-scored variant that is the point of the rung. The FOLDER
            LEAF rung queries the tail behind a shelf prefix ("RO07 - Sandqueen") or
            else the whole leaf name, and is SCORED against that leaf only when the
            tag title does not contradict it (empty, spells the leaf out, or is a
            bare shortcode with no real word - leafScorable); a mis-shelved folder
            naming another book by the same author otherwise mints a confident match,
            and a search verdict becomes books.work_id, which the contributing stage
            attaches sidecars to. A wider query also retrieves the right SERIES and
            the wrong volume - titleTokens drops pure numbers, so book 1's card is a
            perfect title match for "The Wandering Inn - 7" - so an accept is VETOED
            when the card's series position disagrees with the volume the book claims
            (its series-position tag, else a trailing number or "Book N" marker in the
            title), or when the accepting rung dropped a number the scored title
            carried and the card states no position at all; the walk then continues.
            NARRATOR EVIDENCE is not a second pass: the book's narrators ride the
            match.Query and each card's ride its match.Book, and pkg/match's person
            gate accepts author<->author, query-author<->card-narrator and
            query-narrator<->card-author in ONE Best call (a shelf routinely credits
            the narrator as the author). Coverage.ApplyContributed is
            the read-time repair of a FROZEN verdict: it is resolved once per scan
            and cached, so a work this daemon has since contributed kept reporting
            "needed" - GET /scans folds the contributions table through
            store.LandedCoverage (merged/already_covered only) and patches a KNOWN
            verdict additively, and ONLY when the book's work_id agrees with the
            verdict's (contributions made under another work - a core-flow slug, a
            later manual match - must not stamp this work's badges); the book view
            applies the same patch so the two endpoints cannot disagree. Scans STREAM:
            the manager drives pkg/scan's OnProgress/OnBook hooks, books appear
            incrementally (identity provisional until done - the corroborated,
            sorted final list replaces the array), coverage resolves in a bounded
            pool gated by precomputed identity fingerprints (a stale worker can
            never clobber a fresh verdict), and List() serves job summaries
            (running + last 10 finished) so a reloaded UI reattaches. The newest
            successful result is atomically cached under the daemon data dir and
            restored after restart (explicit stale-until-rescanned snapshot;
            failed/incomplete scans never replace it). GET /scans dynamically
            joins each candidate's canonical source_path to the store's lightweight
            BookTracking projection, so cached results always identify active/done
            pipeline books without baking volatile queue state into the scan cache.
            Persisted
            candidate_overrides (hide / manual work match, keyed by the CANONICAL
            absolute source_path - scan roots and override paths are resolved via
            the same helper) are applied at scan time and reflected live on
            completed jobs via read-time patches; OverrideService owns the
            validate -> resolve -> persist -> reflect workflow (store injected as
            a PersistFunc, so metaops still never imports store). A patch's
            HIDDEN flag is authoritative, but its COVERAGE is only the verdict as
            of the match, so both it and every resolved verdict carry a monotonic
            generation and snapshotLocked applies the NEWEST - a match reflects
            instantly, and this job's (and every later scan's) resolution
            supersedes it. Upstream gains sidecars over time, so an
            unconditional patch froze a manual match's verdict forever: works
            whose characters/recaps had since merged kept reporting "needed",
            their badges never went green, and "exclude already covered" never
            dropped them - through rescans AND restarts, since restoreCache
            re-seeds the patch from the cached snapshot. LIBRARY SIGHTINGS back
            the Library tab's New view: a completed scan records every candidate
            source_path through an INJECTED SightingRecorder (WithSightings). That
            interface is write-plus-one-question (RecordSightings + HasSightings),
            nothing store-shaped crosses it, so *store.DB satisfies it DIRECTLY -
            no adapter, and metaops still never imports store. The recording runs
            BEFORE the job reports done: a client stops polling at done, so its
            last poll must already see the flags. NOTHING sighting-shaped rides the
            job snapshot or the cache - first_seen_at and is_new are attached by
            the API on every read. The BASELINE rule is the upgrade guard: the
            first batch ever recorded is stored as baseline (never new), seeded at
            startup from the restored scan cache (stamped with the cached job's
            started_at) and otherwise by the first completed scan - without it an
            upgrade would report the user's entire library as new. is_new itself is
            NOT stored: ComputeIsNew(book, baseline, acknowledged) is a read-time
            predicate (not baseline, not acknowledged, no pipeline_book, not
            hidden) because three of those four change without a rescan; the API
            calls it only for a path that HAS a sighting row, since a path with
            none is never new. Deps: stdlib
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
            new endpoints have allowed + denied auth tests. The Library New-view
            round added POST /library/sightings/acknowledge (204; unknown paths
            ignored) and made GET /scans/{id} attach first_seen_at + is_new beside
            the pipeline_book join - one store.ListSightings read per poll (skipped
            entirely for a job with no books, like the anyTracked guard beside it),
            the predicate itself in metaops.ComputeIsNew (api stays
            transport-only). The store is the ONLY home of that state: the scan
            snapshot and its cache carry neither field.
  web/      go:embed of the SPA (build-tag selected) + SPA-fallback static serving
  server/   http.Server wiring, graceful shutdown, the startup banner
web/          the SPA: Vite + React 19 + TS + Tailwind v4 (npm, Node 24); dist/ is embedded
              src/lib/ holds pure, vitest-tested logic (apiClient, candidates, books,
              pipelineState, recentRoots, tabNavigation, useEventStream, scanStore; M6 added timeline,
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
              `?tab=<id>` is the refresh-safe/deep-linkable active tab and follows
              browser back/forward navigation;
              sign-out calls scanStore.reset(). API calls key books by the
              daemon-computed absolute source_path (NEVER a client-side join);
              the relative path is display/selection only. Scan candidates with a
              pipeline_book match stay visible with an In queue/Completed badge but
              have no selection or rematch control; select-all skips them, and a
              successful enqueue patches the row immediately before the next poll.
              scanStore also holds a `view: 'new' | 'all'` filter DEFAULTING to
              'new' (over the daemon's per-book is_new flag), with a "Dismiss" bulk
              action posting the selected source paths to
              POST /library/sightings/acknowledge.
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
`pipeline -> {audio, asr, transcript, ebook, qa, spelling, agent, repair, toolfetch,
scratch, secrets, fsutil, metaops, store, state, scheduler}` (metaops since M7's
contributing stage, and the spelling stage's series glossary); `agent`/`repair` are leaf helpers (no
scheduler/store deps; `repair -> qa` for the shared Python-compat gram/repr helpers
and the shared clip-floor predicate `qa.ClipStartInRange`/`qa.ClipStartFloorSec`);
`state` is pure. Handlers marshal DTOs and call into the injected packages; they
hold no logic (state transitions live in `state`, dispatch in `scheduler`).
