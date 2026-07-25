// Command audiosilo-bench prepares private replay corpora, runs isolated model
// profiles through the real post-ASR pipeline, and summarizes measured results.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/benchmark"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "prepare":
		fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
		specPath := fs.String("spec", "", "private suite specification YAML")
		out := fs.String("out", "", "new private suite directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *specPath == "" || *out == "" {
			return fmt.Errorf("prepare requires --spec and --out")
		}
		spec, err := benchmark.LoadSuiteSpec(*specPath)
		if err != nil {
			return err
		}
		suite, err := benchmark.Prepare(spec, *out, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("prepared %d cases at %s\n", len(suite.Cases), filepath.Join(*out, "suite.yaml"))
		return nil
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		suite := fs.String("suite", "", "prepared suite.yaml")
		matrix := fs.String("matrix", "benchmarks/codex-matrix.yaml", "profile matrix YAML")
		results := fs.String("results", "", "private results directory")
		profile := fs.String("profile", "all", "profile id or all")
		caseID := fs.String("case", "all", "case id or all")
		repeat := fs.Int("repeat", 1, "repetitions per profile/case")
		seed := fs.Int64("seed", 20260719, "reproducible randomized execution-order seed")
		timeout := fs.Duration("timeout", 60*time.Minute, "per agent invocation timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *suite == "" || *results == "" {
			return fmt.Errorf("run requires --suite and --results")
		}
		_, err := benchmark.Run(ctx, benchmark.RunOptions{SuitePath: *suite, MatrixPath: *matrix, ResultsDir: *results, ProfileID: *profile, CaseID: *caseID, Repeat: *repeat, Seed: *seed, Timeout: *timeout, Out: os.Stdout})
		return err
	case "report":
		fs := flag.NewFlagSet("report", flag.ContinueOnError)
		results := fs.String("results", "", "private results directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *results == "" {
			return fmt.Errorf("report requires --results")
		}
		report, err := benchmark.BuildReport(*results, time.Now())
		if err != nil {
			return err
		}
		if err := benchmark.WriteReport(*results, report); err != nil {
			return err
		}
		fmt.Printf("wrote %s and %s\n", filepath.Join(*results, "report.json"), filepath.Join(*results, "report.md"))
		return nil
	case "calibrate":
		fs := flag.NewFlagSet("calibrate", flag.ContinueOnError)
		suite := fs.String("suite", "", "prepared suite.yaml")
		matrix := fs.String("matrix", "benchmarks/codex-matrix.yaml", "profile matrix YAML")
		results := fs.String("results", "", "new private calibration results directory")
		profile := fs.String("profile", "", "one profile whose holdout judges are used")
		caseID := fs.String("case", "", "one case whose accepted reference is judged")
		timeout := fs.Duration("timeout", 60*time.Minute, "per judge invocation timeout")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		result, err := benchmark.Calibrate(ctx, benchmark.RunOptions{SuitePath: *suite, MatrixPath: *matrix, ResultsDir: *results, ProfileID: *profile, CaseID: *caseID, Timeout: *timeout, Out: os.Stdout})
		if err != nil {
			return err
		}
		passed := 0
		for _, holdout := range result.Holdouts {
			if holdout.Passed {
				passed++
			}
		}
		fmt.Printf("accepted reference passed %d/%d holdout judges\n", passed, len(result.Holdouts))
		return nil
	case "history":
		fs := flag.NewFlagSet("history", flag.ContinueOnError)
		db := fs.String("db", "", "sidecars.db SNAPSHOT (never the live daemon DB)")
		out := fs.String("out", "", "output directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *db == "" || *out == "" {
			return fmt.Errorf("history requires --db and --out")
		}
		if err := os.MkdirAll(*out, 0o750); err != nil {
			return err
		}
		report, err := benchmark.BuildHistory(ctx, *db, time.Now())
		if err != nil {
			return err
		}
		if err := benchmark.WriteHistory(*out, report); err != nil {
			return err
		}
		fmt.Printf("wrote historical telemetry for %d invocations\n", report.Invocations)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `audiosilo-bench - private sidecar agent evaluation harness

Commands:
  prepare --spec SPEC.yaml --out PRIVATE_SUITE_DIR
  run --suite PRIVATE_SUITE_DIR/suite.yaml --matrix MATRIX.yaml --results DIR [--profile ID] [--case ID] [--repeat N]
  report --results DIR
  calibrate --suite PRIVATE_SUITE_DIR/suite.yaml --matrix MATRIX.yaml --results NEW_DIR --profile ID --case ID
  history --db SNAPSHOT.db --out DIR`)
}
