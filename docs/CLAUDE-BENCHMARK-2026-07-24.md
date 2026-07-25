# Claude sidecar benchmark: initial findings

Started: 2026-07-24

Completed: 2026-07-25

Claude Code: 2.1.214

Resolved aliases: Haiku 4.5 for the authentication smoke; the result files record
the CLI alias and invocation metadata for every production-like run.

Corpus: the same frozen `bobiverse-1` input and SHA-256 used by the Codex screen

## Decision

Do not change production routing to a Claude profile from this screen. No Claude
profile passed every hard gate, so the Pareto frontier is empty.

The result is more specific than "Claude is worse" or "use Opus everywhere":

- Haiku everywhere completed cheaply but was semantically unsafe. Its own loop
  passed output that Sol/high rejected with 10 BLOCKER and 15 FIX findings.
- Sonnet was reliable at fact extraction and generally obeyed strict audit JSON
  better than Opus, but all-Sonnet did not converge within three fix rounds.
- Opus produced the strongest first-pass synthesis when given Sonnet or Opus facts,
  but repeated audit rounds frequently emitted progress metadata outside the strict
  `audit.json` schema.
- Opus everywhere completed and converged internally, but Sol/high still found
  seven blockers and eight fixes. A same-provider Opus/high holdout found only one
  fix, demonstrating substantial judge-family bias.
- A targeted Sonnet-audit/Opus-fix hybrid reduced Opus audit-schema exposure but
  still exhausted schema retries on its third audit.

The current bottleneck is therefore the audit/fix protocol and reveal-safe
contract, not simply the choice of Claude tier. The production Terra/Sol route
remains the provisional closest result from the frozen case, although it too
failed its earlier strict holdout and is not a benchmark winner.

## Experimental block

Every profile used the real post-ASR pipeline, a fresh work tree and SQLite
database, the same input digest, seed `20260719`, fact fan-out three, medium
generation effort, and independent Sol/high plus Opus/high holdouts when the
candidate reached that stage.

The first five Claude routes were:

- Haiku for every agent stage;
- Haiku spelling/facts, then Opus synthesis/audit/fix;
- Sonnet for every agent stage;
- Sonnet spelling/facts, then Opus synthesis/audit/fix;
- Opus for every agent stage.

A follow-up route used Sonnet spelling/facts, Opus synthesis, Sonnet audit, and
Opus fix. It was selected from the measured stage-level behavior rather than
added to the original grid retrospectively.

One representative case can reject a route with hard failures. It cannot
establish small cost, latency, or quality differences between survivors. Because
there were no survivors, no other books or repetitions were run.

### Source-snapshot caveat

Pipeline and embedded prompt files were modified by another workspace task at
approximately 22:14-22:18 local time. Each `go run` embeds the prompts when its
binary is compiled:

- calibration and all-Haiku began before those edits;
- the remaining original profiles and targeted hybrid began after the relevant
  pipeline/prompt edits.

All-Haiku is therefore a separate functional rejection block, not a clean
latency comparison with the later profiles. The later Claude profiles share the
same relevant prompt/pipeline snapshot. The harness already records corpus,
matrix, pricing and CLI metadata; future work should additionally record a
source/prompt fingerprint so dirty-worktree changes are machine-detectable.

Production Codex activity overlapped the all-Opus Sol holdout. That holdout's
quality findings remain usable, but its latency is excluded from speed
conclusions. Claude generation calls were kept sequential.

## Results

Costs below are generation-pipeline costs reported by Claude Code. They exclude
holdout judges. The API proxy is retained as a separately versioned estimate; it
materially understates these runs because the stored proxy inputs do not capture
Claude's large cache-creation charges.

| Route | Complete | Hard outcome | Wall min | Reported generation cost | API proxy | Audit / fix | Character recall |
|---|---:|---|---:|---:|---:|---:|---:|
| Haiku all | yes | holdouts failed | 50.2 | $3.22 | $1.43 | 3 / 2 | 60.6% |
| Haiku → Opus | no | audit schema retries exhausted | 35.7* | $8.62 | $3.41 | 2 / 2 | n/a |
| Sonnet all | no | non-convergent after three fixes | 43.8* | $12.11 | $5.86 | 4 / 3 | 84.8% |
| Sonnet → Opus | no | audit schema retries exhausted | 36.0* | $11.62 | $5.43 | 2 / 2 | n/a |
| Opus all | yes | holdouts failed | 46.9 | $12.96 | $5.98 | 3 / 2 | 75.8% |
| Sonnet → Opus synth → Sonnet audit → Opus fix | no | audit schema retries exhausted | 33.2* | $10.20 | $4.80 | 2 / 2 | n/a |

`*` A failed run stops at its hard failure, so its wall time is not comparable to
a profile that completed both holdouts.

Across the six generation runs, Claude Code reported $58.73 of candidate spend.
The two completed profiles' Opus holdouts added $4.02. Including the Opus
calibration and the minimal Haiku authentication probe, the Claude-reported
total for this research block was about $64.78. Sol subscription usage is not a
provider-reported dollar charge and is not included.

## Accuracy findings

### Haiku everywhere

Haiku needed two mechanical retries, three audits and two semantic fixes, then
declared the output clean. Reference-name recall was only 60.6%.

Sol/high subsequently found ten blockers and fifteen fixes, including:

- later personality and job details folded into reveal cards;
- later population, social-structure and burial facts on early cards;
- plasma weapons and revenge decisions placed many chapters early;
- inaccurate role and naming-origin claims;
- eight omitted recurring or consequential characters.

Opus/high found five fixes and three nits. It agreed on several reveal leaks but
was much less severe than Sol. All-Haiku is rejected without further repeats.

### Sonnet everywhere

Sonnet produced 35 cards and 84.8% reference-name recall, the best stability
diagnostic among the Claude profiles that reached scoring. It also found a broad
first-pass defect set. However, findings continued across four audits and three
fixes. The final audit still contained:

- Kenneth Martins's chapter-10 outcome on a chapter-8 card;
- Dr. Doucette's later ongoing role on a chapter-11 card;
- the unsupported claim that Andrea was Bob's twin.

Some later problems were repair-induced: an auditor requested missing cards
while its suggestion included future details, and the fixer copied those details
into reveal-position cards. This is evidence against treating auditor
suggestions as inherently reveal-safe.

### Sonnet facts with Opus synthesis

This combination had the strongest initial mixed-model synthesis. The first
Opus audit found only two FIX-level recap leaks. Subsequent Opus audit attempts,
however, emitted unknown metadata fields such as `notes`, `verified_round`,
`summary`, and `prior_text`; the profile exhausted its strict schema retries.

The targeted Sonnet-audit/Opus-fix follow-up avoided those exact Opus fields for
two rounds and found a broader reveal/coverage set, but its third Sonnet audit
then emitted `round`, `checked`, and `summary` and also exhausted retries.
Changing the audit model alone does not solve the protocol defect.

### Opus everywhere

All-Opus had the best initial self-audit result: one FIX and one NIT. After two
repairs it converged internally. It also had the highest generation cost and no
extraction or synthesis speed advantage.

The independent Sol/high holdout found seven blockers and eight fixes:

- five reveal-position character leaks;
- migration events stated two to twelve chapters early;
- Julia incorrectly described as Bob's descendant;
- Beta Hydri incorrectly called the first proof of intelligent extraterrestrial
  life despite the earlier Deltans;
- production-facing "chunk" language;
- missing consequential supporting characters.

Opus/high found one fix and four nits on the same final files. Both judges found
the chapter-36 migration leak, but Sol found many additional defects. Opus
auditing Opus output is not sufficiently independent for a hard quality gate.

## Judge calibration

The accepted reference passed neither judge:

| Judge | Calibration result |
|---|---:|
| Sol/high | 0 BLOCKER, 4 FIX |
| Opus/high | 0 BLOCKER, 2 FIX, 2 NIT |

Both judges found the two stale `sources` arrays. Both also noticed the chapter-2
competition wording problem, but Sol promoted the two occurrences to FIX while
Opus treated the underlying wording as a NIT. This establishes a real severity
baseline: Sol is stricter, but the large candidate failures cannot be dismissed
as only the two known stale-reference findings.

## Cost and speed findings

- Haiku is cheaper per call, but retries and long tool loops made all-Haiku the
  slowest completed profile.
- Sonnet spelling failed the first reveal gate in all three Sonnet-front runs.
  Initial attempts took about six to eight minutes and were followed by short
  targeted repairs. This stage needs protocol redesign, not a larger model.
- Sonnet fact extraction was consistent: partitions generally completed in
  roughly 95-185 seconds without validation failures, but cost about
  $0.54-$0.69 each.
- Opus fact extraction was slower and roughly $1 per partition, with no measured
  reliability advantage.
- Opus synthesis passed mechanically on the first attempt in every sampled route.
  It cost about $1.44-$1.66 and took roughly 4-6 minutes.
- Opus audit output repeatedly drifted outside the strict schema on later rounds.
  Sonnet reduced but did not eliminate this behavior.

Wall-time rankings should not be overinterpreted: incomplete routes stop early,
and the all-Opus Sol holdout was contaminated by production Codex work.

## Recommended next experiment

Do not spend credits repeating the current profiles. First change the audit
protocol and create a narrow schema-conformance suite:

1. Separate open findings from progress/verification metadata. The strict
   artifact should contain only current `BLOCKER`, `FIX`, and `NIT` findings.
2. Deterministically discard or store harmless unknown top-level audit metadata
   outside `audit.json`, while continuing to reject unknown severities or
   malformed required finding fields. Do not silently hide semantic content.
3. Rewrite fix instructions so a suggestion that requests a new reveal-position
   card cannot carry later-chapter facts into that card.
4. Run 20-30 cheap repeated audit-verification microcases before another
   whole-book test; measure schema-pass rate and open-finding recall separately.
5. Record a source/prompt fingerprint in every result.
6. Once the protocol passes the micro-suite, rerun only:
   - Sonnet facts → Opus synthesis → Sonnet audit → Opus fix;
   - Sonnet everywhere as the cheaper control;
   - the existing Terra → Sol baseline under the same source snapshot and both
     holdout judges.

Only profiles that complete, validate, converge and pass both independent
holdouts should advance to three repetitions across all three frozen books.

## Artifacts

- Consolidated report:
  `~/.audiosilo-bench/results-cross-provider-smoke-20260724/report.md`
- Machine-readable report:
  `~/.audiosilo-bench/results-cross-provider-smoke-20260724/report.json`
- Judge calibration:
  `~/.audiosilo-bench/calibration-cross-provider-20260724/`
- Per-run artifacts:
  `~/.audiosilo-bench/results-cross-provider-smoke-20260724/<profile>/bobiverse-1/r01/`

