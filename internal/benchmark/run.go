package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/agent"
	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"github.com/kodestar/audiosilo-sidecars/internal/pipeline"
	"github.com/kodestar/audiosilo-sidecars/internal/scheduler"
	"github.com/kodestar/audiosilo-sidecars/internal/secrets"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

type RunOptions struct {
	SuitePath  string
	MatrixPath string
	ResultsDir string
	ProfileID  string
	CaseID     string
	Repeat     int
	Seed       int64
	Timeout    time.Duration
	Out        *os.File
	// RunnerFactory is a test seam. Production leaves it nil and resolves the
	// profile's real headless CLI through agent.Select.
	RunnerFactory func(context.Context, string, string) (agent.Runner, agent.Availability, error)
}

type RunResult struct {
	Version     int                     `json:"version"`
	Profile     string                  `json:"profile"`
	Case        string                  `json:"case"`
	Repeat      int                     `json:"repeat"`
	Backend     string                  `json:"backend"`
	Models      map[string]string       `json:"models"`
	Efforts     map[string]string       `json:"efforts,omitempty"`
	Fanout      int                     `json:"max_agents_per_book"`
	Seed        int64                   `json:"seed"`
	InputSHA256 string                  `json:"input_sha256"`
	Pricing     string                  `json:"pricing_version,omitempty"`
	CLIPath     string                  `json:"cli_path,omitempty"`
	CLIVersion  string                  `json:"cli_version,omitempty"`
	StartedAt   string                  `json:"started_at"`
	FinishedAt  string                  `json:"finished_at"`
	WallSeconds float64                 `json:"wall_seconds"`
	Completed   bool                    `json:"completed"`
	Error       string                  `json:"error,omitempty"`
	AuditRounds int                     `json:"audit_rounds"`
	FixRounds   int                     `json:"fix_rounds"`
	Stages      []store.StageRun        `json:"stages"`
	Invocations []store.AgentInvocation `json:"invocations"`
	Quality     Quality                 `json:"quality"`
	Holdouts    []HoldoutResult         `json:"holdouts,omitempty"`
}

type HoldoutResult struct {
	Backend    string                  `json:"backend"`
	Model      string                  `json:"model"`
	Effort     string                  `json:"effort,omitempty"`
	CLIPath    string                  `json:"cli_path,omitempty"`
	CLIVersion string                  `json:"cli_version,omitempty"`
	Passed     bool                    `json:"passed"`
	Blockers   int                     `json:"blockers"`
	Fixes      int                     `json:"fixes"`
	Nits       int                     `json:"nits"`
	Error      string                  `json:"error,omitempty"`
	Invocation []store.AgentInvocation `json:"invocations,omitempty"`
}

func Run(ctx context.Context, opts RunOptions) ([]RunResult, error) {
	if strings.TrimSpace(opts.ResultsDir) == "" {
		return nil, fmt.Errorf("results directory is empty")
	}
	suite, err := LoadSuite(opts.SuitePath)
	if err != nil {
		return nil, err
	}
	matrix, err := LoadMatrix(opts.MatrixPath)
	if err != nil {
		return nil, err
	}
	if opts.Repeat < 1 {
		opts.Repeat = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Minute
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	profiles := selectProfiles(matrix, opts.ProfileID)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("profile %q not found (available: %s)", opts.ProfileID, strings.Join(sortedProfileIDs(matrix), ", "))
	}
	cases := selectCases(suite, opts.CaseID)
	if len(cases) == 0 {
		return nil, fmt.Errorf("case %q not found", opts.CaseID)
	}
	base := filepath.Dir(opts.SuitePath)
	for _, c := range cases {
		input := filepath.Join(base, filepath.FromSlash(c.InputDir))
		digest, derr := treeDigest(input)
		if derr != nil {
			return nil, derr
		}
		if digest != c.SHA256 {
			return nil, fmt.Errorf("case %q input digest changed: got %s, want %s", c.ID, digest, c.SHA256)
		}
	}
	type task struct {
		repeat int
		c      PreparedCase
		p      Profile
	}
	var tasks []task
	for repeat := 1; repeat <= opts.Repeat; repeat++ {
		for _, c := range cases {
			for _, p := range profiles {
				tasks = append(tasks, task{repeat: repeat, c: c, p: p})
			}
		}
	}
	// Reproducible randomization reduces confounding from quota pressure, provider
	// load, and machine temperature without introducing concurrent-run contention.
	rand.New(rand.NewSource(opts.Seed)).Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] }) //nolint:gosec // experimental ordering, not security
	var results []RunResult
	var taskErrs []error
	for _, task := range tasks {
		fmt.Fprintf(opts.Out, ">> benchmark %s / %s / r%d\n", task.p.ID, task.c.ID, task.repeat)
		result, rerr := runOne(ctx, opts, matrix, task.p, task.c, base, task.repeat)
		results = append(results, result)
		if rerr != nil {
			fmt.Fprintf(opts.Out, "!! %s / %s: %v\n", task.p.ID, task.c.ID, rerr)
			taskErrs = append(taskErrs, fmt.Errorf("%s / %s / r%d: %w", task.p.ID, task.c.ID, task.repeat, rerr))
		} else {
			fmt.Fprintf(opts.Out, "<< %s / %s complete in %.1fm\n", task.p.ID, task.c.ID, result.WallSeconds/60)
		}
	}
	return results, errors.Join(taskErrs...)
}

func runOne(ctx context.Context, opts RunOptions, matrix Matrix, profile Profile, c PreparedCase, suiteBase string, repeat int) (result RunResult, retErr error) {
	started := time.Now()
	result = RunResult{
		Version: schemaVersion, Profile: profile.ID, Case: c.ID, Repeat: repeat,
		Backend: profile.Backend, Models: profile.Models, Efforts: profile.Efforts,
		Fanout: profile.MaxAgentsPerBook, Seed: opts.Seed, InputSHA256: c.SHA256,
		Pricing: matrix.Pricing.Version, StartedAt: started.UTC().Format(time.RFC3339Nano),
	}
	runDir := filepath.Join(opts.ResultsDir, profile.ID, c.ID, fmt.Sprintf("r%02d", repeat))
	if err := ensureNewDir(runDir); err != nil {
		return result, err
	}
	defer func() {
		result.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		result.WallSeconds = time.Since(started).Seconds()
		if retErr != nil {
			result.Error = retErr.Error()
		}
		if err := writeJSON(filepath.Join(runDir, "result.json"), result); err != nil && retErr == nil {
			retErr = err
		}
	}()

	workDir := filepath.Join(runDir, "work")
	if err := copyTree(filepath.Join(suiteBase, filepath.FromSlash(c.InputDir)), workDir); err != nil {
		return result, err
	}
	db, err := store.Open(ctx, filepath.Join(runDir, store.FileName))
	if err != nil {
		return result, err
	}
	defer func() { _ = db.Close() }()
	book, err := db.CreateBook(ctx, store.NewBook{
		SourcePath: "benchmark://" + profile.ID + "/" + c.ID + fmt.Sprintf("/%d", repeat),
		WorkDir:    workDir, Title: c.Title, Authors: c.Authors, Series: c.Series,
		SeriesPos: c.SeriesPos, WorkID: c.WorkID, State: string(state.SpellingResearch),
	})
	if err != nil {
		return result, err
	}
	selectCfg, models, efforts := backendConfig(profile.Backend, profile.CLIPath, profile.Models, profile.Efforts)
	runner, av, err := resolveRunner(ctx, opts, selectCfg, profile.CLIPath)
	if err != nil {
		return result, err
	}
	if runner == nil || !av.Available {
		return result, fmt.Errorf("%s runner unavailable: %s", profile.Backend, av.Detail)
	}
	result.CLIPath, result.CLIVersion = av.Path, av.Version
	exe := pipeline.NewExecutor(pipeline.Config{
		DB: db, DataDir: runDir, Agent: runner, AgentAvail: av,
		AgentSelect: selectCfg,
		AgentModels: models, AgentEfforts: efforts, AgentTimeout: opts.Timeout,
		AgentConcurrency: profile.MaxAgentsPerBook, MaxAgentsPerBook: profile.MaxAgentsPerBook,
		BookBudgetUSD: 0, Pricing: matrix.Pricing, Secrets: secrets.NewMemStore(),
		Log: slog.New(slog.NewTextHandler(opts.Out, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	stage := state.SpellingResearch
	fixAttempts := 0
	for stage != state.Ready {
		stageResult, err := executeStage(ctx, db, exe, book, stage)
		if err != nil {
			populateResult(ctx, db, book.ID, &result)
			return result, err
		}
		switch stage {
		case state.Auditing:
			result.AuditRounds++
		case state.Fixing:
			fixAttempts++
			result.FixRounds++
		}
		next, status, err := state.NextState(stage, stageResult.Outcome(fixAttempts))
		if err != nil {
			populateResult(ctx, db, book.ID, &result)
			return result, err
		}
		if status == state.StatusNeedsAttention {
			populateResult(ctx, db, book.ID, &result)
			return result, fmt.Errorf("audit did not converge after %d fix rounds", fixAttempts)
		}
		stage = next
	}
	result.Completed = true
	result.Quality = Score(workDir, filepath.Join(suiteBase, filepath.FromSlash(c.Reference)))
	populateResult(ctx, db, book.ID, &result)
	for i, judge := range profile.Judges {
		holdout, err := runHoldout(ctx, opts, matrix, c, profile, judge, workDir, runDir, i)
		result.Holdouts = append(result.Holdouts, holdout)
		if err != nil {
			return result, fmt.Errorf("holdout judge %d: %w", i+1, err)
		}
	}
	return result, nil
}

func executeStage(ctx context.Context, db *store.DB, exe *pipeline.Executor, book store.Book, stage state.State) (scheduler.StageResult, error) {
	attempt, err := db.CountStageRuns(ctx, book.ID, string(stage))
	if err != nil {
		return scheduler.StageResult{}, err
	}
	runID, err := db.StartStageRun(ctx, book.ID, string(stage), attempt+1)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	result, runErr := exe.Execute(ctx, book, stage, scheduler.StageReport{})
	metrics := result.Metrics
	if runErr != nil {
		metrics, _ = json.Marshal(map[string]string{"error": runErr.Error()})
	}
	if err := db.FinishStageRun(context.WithoutCancel(ctx), runID, runErr == nil, metrics); err != nil && runErr == nil {
		runErr = err
	}
	return result, runErr
}

func runHoldout(ctx context.Context, opts RunOptions, matrix Matrix, c PreparedCase, profile Profile, j Judge, sourceWork, runDir string, index int) (HoldoutResult, error) {
	out := HoldoutResult{Backend: j.Backend, Model: j.Model, Effort: j.Effort}
	judgeRoot := filepath.Join(runDir, fmt.Sprintf("holdout-%02d-%s", index+1, j.Backend))
	if err := os.MkdirAll(judgeRoot, 0o750); err != nil {
		return out, err
	}
	judgeWork := filepath.Join(judgeRoot, "work")
	if err := os.MkdirAll(judgeWork, 0o750); err != nil {
		return out, err
	}
	if err := copySelected(sourceWork, judgeWork,
		[]string{"manifest.json", "spellings.json", "validation_report.json"}, []string{"facts", "sidecars"}, true); err != nil {
		return out, err
	}
	db, err := store.Open(ctx, filepath.Join(judgeRoot, store.FileName))
	if err != nil {
		return out, err
	}
	defer func() { _ = db.Close() }()
	book, err := db.CreateBook(ctx, store.NewBook{SourcePath: "benchmark-judge://" + profile.ID + "/" + c.ID,
		WorkDir: judgeWork, Title: c.Title, Authors: c.Authors, Series: c.Series, SeriesPos: c.SeriesPos, WorkID: c.WorkID, State: string(state.Auditing)})
	if err != nil {
		return out, err
	}
	modelRoutes := map[string]string{string(state.Auditing): j.Model}
	effortRoutes := map[string]string{string(state.Auditing): j.Effort}
	selectCfg, models, efforts := backendConfig(j.Backend, j.CLIPath, modelRoutes, effortRoutes)
	runner, av, err := resolveRunner(ctx, opts, selectCfg, j.CLIPath)
	if err != nil {
		return out, err
	}
	if runner == nil || !av.Available {
		return out, fmt.Errorf("%s unavailable: %s", j.Backend, av.Detail)
	}
	out.CLIPath, out.CLIVersion = av.Path, av.Version
	exe := pipeline.NewExecutor(pipeline.Config{DB: db, DataDir: judgeRoot, Agent: runner, AgentAvail: av,
		AgentSelect: selectCfg, AgentModels: models, AgentEfforts: efforts, AgentTimeout: opts.Timeout, AgentConcurrency: 1,
		MaxAgentsPerBook: 1, BookBudgetUSD: 0, Pricing: matrix.Pricing, Secrets: secrets.NewMemStore()})
	res, err := executeStage(ctx, db, exe, book, state.Auditing)
	inv, _ := db.ListAgentInvocations(ctx, book.ID)
	out.Invocation = inv
	if err != nil {
		out.Error = err.Error()
		return out, err
	}
	out.Passed = res.AuditPassed
	var report struct {
		Findings []struct {
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	raw, rerr := os.ReadFile(filepath.Join(judgeWork, "audit.json")) //nolint:gosec // isolated benchmark path
	if rerr == nil && json.Unmarshal(raw, &report) == nil {
		for _, f := range report.Findings {
			switch f.Severity {
			case "BLOCKER":
				out.Blockers++
			case "FIX":
				out.Fixes++
			case "NIT":
				out.Nits++
			}
		}
	}
	return out, nil
}

func resolveRunner(ctx context.Context, opts RunOptions, cfg agent.SelectConfig, path string) (agent.Runner, agent.Availability, error) {
	if opts.RunnerFactory != nil {
		return opts.RunnerFactory(ctx, cfg.Backend, path)
	}
	return agent.Select(ctx, cfg, secrets.NewMemStore())
}

func backendConfig(backend, path string, modelRoutes, effortRoutes map[string]string) (agent.SelectConfig, pipeline.AgentModels, pipeline.AgentEfforts) {
	selectCfg := agent.SelectConfig{Backend: backend}
	models := pipeline.AgentModels{}
	efforts := pipeline.AgentEfforts{}
	if backend == agent.IDClaude {
		selectCfg.ClaudePath = path
		models.Claude, efforts.Claude = modelRoutes, effortRoutes
	} else {
		selectCfg.CodexPath = path
		models.OpenAI, efforts.OpenAI = modelRoutes, effortRoutes
	}
	return selectCfg, models, efforts
}

func selectProfiles(m Matrix, id string) []Profile {
	if id == "" || id == "all" {
		return m.Profiles
	}
	for _, p := range m.Profiles {
		if p.ID == id {
			return []Profile{p}
		}
	}
	return nil
}

func selectCases(s Suite, id string) []PreparedCase {
	if id == "" || id == "all" {
		return s.Cases
	}
	for _, c := range s.Cases {
		if c.ID == id {
			return []PreparedCase{c}
		}
	}
	return nil
}

func populateResult(ctx context.Context, db *store.DB, bookID int64, result *RunResult) {
	result.Stages, _ = db.ListStageRuns(ctx, bookID)
	result.Invocations, _ = db.ListAgentInvocations(ctx, bookID)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(raw, '\n'), 0o600)
}
