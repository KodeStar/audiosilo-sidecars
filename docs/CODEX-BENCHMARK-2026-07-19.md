# Codex sidecar benchmark: initial findings

Started: 2026-07-19

Updated: 2026-07-20

Codex CLI: 0.144.5

Corpus digest: recorded in the private prepared suite

## Decision so far

Do not route the entire pipeline to Luna. On the representative Bobiverse case,
the all-Luna profile completed and passed mechanical validation, but failed the
independent Sol/high holdout with three BLOCKER findings and three FIX findings.
The failures included future identity/location information and a recap event
placed before it was established. This is a hard quality-gate failure, not a
minor difference in style.

The production Terra/Sol split remains the operational baseline until the paired
screen finishes. Luna for bounded spelling research and fact extraction, with Sol
retained for synthesis, audit, and fixes, was substantially safer than all-Luna
but still failed its independent holdout. It is therefore a promising optimization
candidate, not a deployable winner.

There is not yet enough randomized evidence to name a final accuracy/cost/speed
sweet spot. A single case can reject an unsafe profile, but small differences
between viable profiles need repeated, paired runs across the corpus.

## Prepared corpus

The private, frozen v1 corpus lives outside the repository and contains three
completed series openers (about 42 hours and 142 chapters total): a 9.5-hour,
61-chapter science-fiction title; a 16.6-hour, 41-chapter LitRPG title; and a
15.9-hour, 40-chapter fantasy title. Together they exercise ordinary names,
invented terminology, dense casts, and very different chapter granularity. This
is enough for screening, but the six-book stratified corpus described in
`BENCHMARKING.md` remains the target before a final deployment decision.

Only normalized/repaired text, manifests, markers, and accepted reference
artifacts were copied. Audio, raw ASR, prior agent runs, databases, and secrets
were excluded. The prepared input contains 259 files (about 3.8 MB), and every
run verifies its recorded SHA-256 before sending work to an agent.

## Controlled all-Luna result

Profile: `luna_all_medium_f3`
Case: `bobiverse-1` (9.5 hours, 61 chapters)
Generation: Luna, medium effort, fact fan-out 3
Holdout: fresh Sol, high effort

| Measure | Result |
|---|---:|
| Pipeline completion | pass |
| Mechanical validation | pass |
| Luna audit rounds | 4 |
| Luna fix rounds | 3 (configured maximum) |
| Validation-failed Luna audit attempts | 2 |
| Independent holdout | **fail: 3 BLOCKER, 3 FIX** |
| Generation input tokens | 7.87M |
| Generation output tokens | 116k |
| Character-name recall vs accepted reference | 84.8% |
| Character-name Jaccard vs accepted reference | 68.3% |
| Recap through-point Jaccard vs accepted reference | 4.8% |
| Wall time | 38.3 minutes, contaminated |
| Summed agent time | 40.5 minutes |
| Luna dollar cost | unknown |

The low recap overlap and character differences are warning signals, not truth
labels: multiple valid sidecars can choose different names and recap boundaries.
The independent audit is the semantic hard gate. It found that Luna's own
iterative audit/fix loop had converged mechanically without converging safely.

The wall time must not be used for model ranking because production Codex jobs
overlapped this smoke run. The generation usage is still valid. No dollar figure
is reported for Luna because the subscription-authenticated CLI reports tokens,
not a provider charge, and no defensible Luna API-equivalent rate is configured.
The Sol holdout used another 548k input and 10.7k output tokens; its configured
API-equivalent estimate was $3.30 and is excluded from candidate generation cost.

## Controlled Luna/Sol result

The paired `luna_sol_medium_f3` run used the same Bobiverse input and seed. Luna
handled spelling and five fact chunks; Sol handled synthesis, the audit/fix loop,
and a fresh Sol/high holdout.

| Measure | Result |
|---|---:|
| Pipeline completion | pass |
| Mechanical validation | pass |
| Audit / fix rounds | 4 / 3 |
| Independent holdout | **fail: 0 BLOCKER, 2 FIX** |
| Generation calls | 19 |
| Generation input / output | 7.93M / 132k |
| Validation-failed calls | 2 |
| Failed/rate-limited calls | 3 |
| Character-name recall | 84.8% |
| Recap through-point Jaccard | 18.8% |
| Wall / summed agent time | 47.8 / 49.4 minutes |

This was a meaningful quality improvement over all-Luna: the independent judge
found no blocker-class spoiler leak. The remaining findings were an inaccurate
ownership phrase in a recap and an omitted recurring character. It was not a
speed or reliability improvement: spelling needed two schema repairs, one fact
chunk needed a full retry, the Sol loop used all three fixes, and two later Sol
calls hit rate limits before retrying. Because Luna has no configured price, the
route still has no defensible dollar total. The result is excluded from the
Pareto frontier because the holdout did not pass.

## Paired route screen

Four medium-effort routes have now run on the same case. All completed and passed
mechanical validation; none passed the strict fresh Sol/high holdout, so none is
eligible for the Pareto frontier.

| Route | Holdout | Wall min* | Input / output | Cost proxy | Audit / fix |
|---|---:|---:|---:|---:|---:|
| Luna all | 3 BLOCKER, 3 FIX | 38.3 | 7.87M / 116k | unknown | 4 / 3 |
| Luna → Sol | 0 BLOCKER, 2 FIX | 47.8 | 7.93M / 132k | unknown | 4 / 3 |
| Terra → Sol | **0 BLOCKER, 1 FIX** | 42.7 | **7.01M / 107k** | **$31.70** | 4 / 3 |
| Sol all | 1 BLOCKER, 0 FIX | 39.8 | 9.67M / 127k | $56.53 | 3 / 2 |

`*` Wall time is contaminated by production agents, rate limiting, and other
machine work; it is included for transparency but not used as clean model latency.

Terra → Sol is the provisional sweet spot among the routes tested: it has the
best holdout outcome, lowest measured token usage, lowest known cost proxy, and
fewer validation repairs than Luna → Sol. It is not yet a deployable benchmark
winner because the holdout still requested one minor-character card. Sol-only is
strong evidence against “use the biggest model everywhere”: it cost about 78%
more than Terra → Sol and still leaked a future project name in a reveal-5 card.

Raising the Terra/Sol tail from medium to high did not close the gap. The high
profile consumed 6.71M input and 124k output tokens ($31.26 configured proxy) in
41.4 contaminated wall minutes, then failed to converge after all three fixes.
Its fourth audit still contained two FIX findings, so no holdout was run. Earlier
rounds also caught reveal-timing blockers introduced during the loop. Higher
reasoning effort therefore increased audit sensitivity and output usage without
producing a stable accepted artifact.

The evidence now points to the audit/fix convergence policy and character-coverage
contract as the bottleneck. Terra → Sol at medium remains the provisional sweet
spot, but “provisional” matters: it is the closest failed route, not a profile that
passed every quality gate. Before changing production routing, calibrate the
holdout against accepted references, improve convergence, and repeat the medium
route across the full corpus. Do not spend more on low-effort testing until a
medium or high profile can pass the semantic gate.

## Holdout calibration

A fresh Sol/high audit of the accepted production reference also failed, with two
FIX findings. Both were deterministic reference drift: `characters.json` and
`recaps.json` contain an extra `{type: community, ref: audiosilo-sidecars}` source,
while the current contract requires exactly `[{"type":"community"}]`. The current
validator enforces that rule, but the frozen reference and its copied validation
report predate the enforcement.

This does not erase the candidates' findings: the Terra/Sol miss was a semantic
character omission, and the Sol-only miss was a reveal-timing spoiler. It does
show why judge calibration is required and why accepted references must be
revalidated when contracts change. Refresh the corpus reference after correcting
the stale source arrays, then run both Sol and Claude holdouts on Monday. Until
then, name/recap overlap remains a stability diagnostic and holdout severity is
more informative than a raw pass percentage.

## Historical production evidence

The read-only snapshot contained 56 books, 26 completed books, and 752 agent
invocations. These data are observational because routing was fixed rather than
randomized. They are useful for estimating stage difficulty and failure modes,
not for claiming that one model caused a difference.

| Stage / route | Calls | Success | Validation failures | Other failures | Mean seconds |
|---|---:|---:|---:|---:|---:|
| spelling / Terra | 35 | 25 | 5 | 5 | 165.6 |
| facts / Terra | 181 | 167 | 0 | 13 | 133.7 |
| synthesis / Sol | 27 | 24 | 0 | 3 | 263.4 |
| audit / Sol | 176 | 133 | 19 | 24 | 100.6 |
| fix / Sol | 105 | 99 | 0 | 6 | 100.2 |
| QA / Terra | 144 | 131 | 3 | 10 | 58.0 |
| markers / Terra | 84 | 5 | 73 | 6 | 56.3 |

The marker validation failures mostly predate parser fixes and should not be read
as present-day model quality. Historical “other failures” include cancellations,
transport errors, and operational failures. Synthesis is the slowest serial
content stage; facts consume the most calls and input, so fact extraction is the
most promising place to save usage if Luna clears the quality gates.

## Remaining Codex screen

The committed matrix tests these stage-routing hypotheses at the same effort and
fact fan-out:

- Luna everywhere (rejected on the first representative case)
- Terra everywhere
- Sol everywhere
- Luna extraction plus Sol synthesis/audit/fix
- Terra extraction plus Sol synthesis/audit/fix (current baseline)
- fact fan-out 1 versus 3 for the Luna/Sol split

After that route screen, `benchmarks/codex-effort-matrix.yaml` varies the Sol tail
across low/medium/high without changing its extraction route. It also probes Luna
extraction at low effort. This staged design avoids a full model × effort grid
while still separating the two main causes of speed and token differences.

Use three repetitions for screening and five for finalists. Execution order is
seeded and randomized; each profile receives byte-identical inputs and a fresh
database/work tree. A profile is eligible for the Pareto frontier only if every
run completes, validates, and passes every holdout. Among eligible profiles the
report preserves the accuracy/latency/usage trade-off rather than inventing a
weighted score.

## Monday cross-provider plan

Run the same frozen corpus and seed using
`benchmarks/cross-provider-matrix.yaml`. It includes Haiku-only, Haiku/Opus,
Sonnet-only, Sonnet/Opus, and Opus-only routes alongside the Codex candidates.
Every result is judged by both Sol/high and Opus/high. This reduces same-family
judge bias; disagreements go to blind human review rather than majority-vote
automation. Haiku is the direct test of Claude's fast/efficient tier in the same
role as Luna; no unsupported Haiku effort setting is applied.

Do not merge measurements across changed prompts, corpus digests, CLI versions,
or materially different machine/provider contention. Re-run the whole paired
block when one of those changes. The exact commands and interpretation rules are
in [BENCHMARKING.md](BENCHMARKING.md).

OpenAI's current model guidance is documented at
<https://learn.chatgpt.com/docs/models>. Model positioning guided the candidate
matrix, but only this workload's measured hard gates determine deployment.
