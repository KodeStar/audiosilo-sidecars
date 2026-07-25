# Agent pipeline benchmarking

`audiosilo-bench` measures the real post-ASR sidecar pipeline against frozen,
private book corpora. It is intended to answer a practical routing question:
which model and reasoning-effort combination gives acceptable sidecars at the
lowest latency and usage, and how much fact-pass fan-out helps?

The benchmark starts after transcript QA. It re-runs spelling research,
mechanical correction, fact extraction and assembly, notes-only synthesis,
mechanical validation, and the complete audit/fix loop. Source audio and ASR are
excluded because they do not depend on the content-agent model. Marker
normalization and QA adjudication need separate decision-labelled suites; do not
mix those classification tasks into the authoring score.

## Experimental design

Use a paired, blocked design: every profile sees byte-identical input for every
book. `prepare` copies only the private normalized/repaired transcript layers and
the accepted reference artifacts, then records an input-tree SHA-256. `run`
verifies the digest before each experiment, uses a fresh work directory and SQLite
database, and runs tasks sequentially in a reproducibly randomized order. Fact
chunks may still fan out within a book according to the profile. Sequential books
avoid measuring provider contention as if it were model latency.

The recommended minimum corpus is six books stratified across:

- short, medium, and long recordings;
- ordinary prose, invented-name-heavy fantasy, and terminology-heavy LitRPG;
- clean ASR and repaired ASR;
- series openers and later volumes once predecessor snapshot support is added;
- sparse and dense casts.

Run three repetitions per book/profile for screening and five for finalists. A
single run is useful operational evidence, but not enough to claim small model
differences. Keep the suite unchanged across providers.

Quality is deliberately layered:

1. Pipeline completion and mechanical validation are hard gates.
2. Audit convergence, fix rounds, validator retries, and `NEEDS AUDIO REVIEW`
   show process reliability.
3. A fresh holdout auditor checks the final sidecars without participating in the
   fix loop. While only Codex is available, use Sol at high effort. Once Claude is
   available, judge every finalist with both Sol and Opus; a same-model judge is
   not independent.
4. Character-name recall and recap-point overlap compare the output with the
   accepted production reference. They are stability diagnostics, not semantic
   correctness scores - different names or through-points can both be valid.
5. Finalists need a blind human comparison using the generated sidecars and
   holdout findings. The harness does not collapse quality, dollars, and seconds
   into a subjective weighted number; it reports the Pareto frontier.

Calibrate every judge against the accepted reference before interpreting a narrow
holdout miss. Keep calibration separate from generation results:

```sh
go run ./cmd/audiosilo-bench calibrate \
  --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
  --matrix benchmarks/codex-matrix.yaml \
  --results ~/.audiosilo-bench/calibration-codex \
  --profile terra_sol_medium_f3 --case short-series-opener
```

If the accepted reference fails, inspect the finding and either correct the
reference/contract or report the judge's measured false-positive baseline. Never
silently weaken a candidate's hard gate to make its score look better.

## Private corpus setup

Never commit the suite. It contains transcripts and fact notes. Put the spec and
prepared data outside the repository:

```sh
mkdir -p ~/.audiosilo-bench
cp benchmarks/suite-spec.example.yaml ~/.audiosilo-bench/suite-spec.yaml
# Edit source_work_dir and metadata for each selected completed book.

go run ./cmd/audiosilo-bench prepare \
  --spec ~/.audiosilo-bench/suite-spec.yaml \
  --out ~/.audiosilo-bench/corpus-v1
```

`prepare` refuses a non-empty destination. It never copies chapter FLACs,
`transcripts-raw/`, `_runs/`, `_done/`, a database, or secrets.

## Codex screening now

The committed Codex matrix controls model and effort per stage. Luna is included
because its documented target includes extraction and structured summaries;
Sol remains the fixed holdout judge.

Start with one representative case to catch harness or entitlement failures:

```sh
go run ./cmd/audiosilo-bench run \
  --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
  --matrix benchmarks/codex-matrix.yaml \
  --results ~/.audiosilo-bench/results-codex-screen \
  --profile luna_all_medium_f3 \
  --case short-series-opener
```

Smoke every profile on one representative case before spending credits on
repetitions. A results directory is append-only by run key; the command refuses
to overwrite an existing profile/case/repetition:

Pause the production batch (or wait until it has no active content-agent
invocations) before measuring. Simultaneous `codex exec`/`claude -p` processes
contend for provider quota and local resources; a run that overlaps them is valid
as a functional smoke test but contaminated for latency and throughput analysis.

```sh
go run ./cmd/audiosilo-bench run \
  --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
  --matrix benchmarks/codex-matrix.yaml \
  --results ~/.audiosilo-bench/results-codex-smoke \
  --case short-series-opener --repeat 1 --seed 20260719

# After rejecting hard-gate failures, replace this example list with the
# survivors and run them across the complete corpus.
for profile in luna_sol_medium_f3 terra_sol_medium_f3 sol_all_medium_f3; do
  go run ./cmd/audiosilo-bench run \
    --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
    --matrix benchmarks/codex-matrix.yaml \
    --results ~/.audiosilo-bench/results-codex-screen \
    --profile "$profile" --repeat 3 --seed 20260719
done

go run ./cmd/audiosilo-bench report \
  --results ~/.audiosilo-bench/results-codex-screen
```

The Luna cost proxy is intentionally `unknown` until a defensible rate is
configured. ChatGPT subscription usage is not a dollar charge reported by the
CLI. Compare measured input/output/cache tokens, wall time, and the account's
credit movement; do not present a made-up dollar conversion as cost.

Once the route screen leaves one or two viable extraction routes, use
`benchmarks/codex-effort-matrix.yaml` as a second experiment. It holds extraction
effort at medium while varying Sol's synthesis/audit/fix effort across low,
medium, and high, and includes one Luna-low extraction probe. Do not mix its
results into the route block: first choose a viable route, then choose the lowest
effort that preserves every hard gate.

## Claude comparison

The initial Claude screen was executed on 2026-07-24/25. See
[`CLAUDE-BENCHMARK-2026-07-24.md`](CLAUDE-BENCHMARK-2026-07-24.md) for the
results, measured spend, judge disagreement, and the targeted follow-up hybrid.
No profile passed every hard gate, so the commands below remain the rerun
procedure after the audit protocol is corrected.

Use the same corpus and seed. The cross-provider matrix has Haiku-only,
Haiku/Opus, Sonnet-only, Sonnet/Opus, and Opus-only generation profiles plus both
Sol and Opus holdout auditors. Haiku has no effort override because Claude Code
does not support one for that model; Sonnet and Opus are fixed at medium for
generation and high for holdout. First smoke all routes on the same case:

```sh
go run ./cmd/audiosilo-bench run \
  --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
  --matrix benchmarks/cross-provider-matrix.yaml \
  --results ~/.audiosilo-bench/results-cross-provider-smoke \
  --case short-series-opener --repeat 1 --seed 20260719

go run ./cmd/audiosilo-bench report \
  --results ~/.audiosilo-bench/results-cross-provider-smoke
```

Then run only the hard-gate survivors over the full corpus. Replace this example
list with the survivors:

```sh
for profile in codex_luna_sol_cross_judged claude_haiku_opus_cross_judged claude_sonnet_opus_cross_judged; do
  go run ./cmd/audiosilo-bench run \
    --suite ~/.audiosilo-bench/corpus-v1/suite.yaml \
    --matrix benchmarks/cross-provider-matrix.yaml \
    --results ~/.audiosilo-bench/results-cross-provider \
    --profile "$profile" --repeat 3 --seed 20260719
done

go run ./cmd/audiosilo-bench report \
  --results ~/.audiosilo-bench/results-cross-provider
```

Do not combine Codex-only results and Monday results if the prompts, CLI versions,
matrix, corpus digest, or machine conditions changed. Record those versions in the
research note and re-run the affected block.

The matrix uses Claude Code's `haiku`, `sonnet`, and `opus` aliases and records the
CLI version and returned invocation model in each result. Alias targets can change,
so a later run is a new block even when the YAML is unchanged. API-equivalent
proxies use Anthropic's published base-input, output, and cache-hit rates; they are
not subscription charges. See Anthropic's [Claude Code model configuration](https://code.claude.com/docs/en/model-config)
and [API pricing](https://platform.claude.com/docs/en/about-claude/pricing).

## Historical baseline

Historical telemetry is useful for failure modes and realistic variance, but it
does not compare models because routing was not randomized. Snapshot the database
while the daemon is running, then analyze only the copy:

```sh
sqlite3 ~/.audiosilo-sidecars/sidecars.db ".backup '$HOME/.audiosilo-bench/history.db'"
go run ./cmd/audiosilo-bench history \
  --db ~/.audiosilo-bench/history.db \
  --out ~/.audiosilo-bench/history-report
```

The history report separates validation failures from transport failures and
reports configured API-equivalent estimates as proxies. Subscription-authenticated
Codex does not report a provider dollar cost.

## Interpreting the sweet spot

Reject a profile if any book fails mechanical validation, does not converge within
the bounded fix loop, or fails either cross-provider holdout audit. Among the
remaining profiles, prefer the non-dominated option with lower median wall time and
usage. Promote a cheaper model stage by stage, beginning with fact extraction and
spelling research, rather than assuming an all-cheap profile is safe. Synthesis and
adversarial audit contain the most open-ended judgment and should be the last stages
demoted from a strong model.
