// Package benchmark runs isolated, repeatable replays of the post-ASR sidecar
// pipeline. Benchmark corpora and results are deliberately local-only: they contain
// audiobook transcripts and derived notes that must never enter the repository.
package benchmark

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-sidecars/internal/pricing"
	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"gopkg.in/yaml.v3"
)

const schemaVersion = 1

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// SuiteSpec points Prepare at private, completed work directories. The prepared
// suite is self-contained, so these source paths do not appear in its manifest.
type SuiteSpec struct {
	Version int        `yaml:"version"`
	Cases   []CaseSpec `yaml:"cases"`
}

type CaseSpec struct {
	ID        string   `yaml:"id"`
	Title     string   `yaml:"title"`
	Authors   []string `yaml:"authors"`
	Series    string   `yaml:"series,omitempty"`
	SeriesPos string   `yaml:"series_pos,omitempty"`
	WorkID    string   `yaml:"work_id,omitempty"`
	SourceDir string   `yaml:"source_work_dir"`
}

// Suite is the portable manifest written by Prepare inside a private corpus.
type Suite struct {
	Version   int            `yaml:"version"`
	CreatedAt string         `yaml:"created_at"`
	Cases     []PreparedCase `yaml:"cases"`
}

type PreparedCase struct {
	ID        string   `yaml:"id"`
	Title     string   `yaml:"title"`
	Authors   []string `yaml:"authors"`
	Series    string   `yaml:"series,omitempty"`
	SeriesPos string   `yaml:"series_pos,omitempty"`
	WorkID    string   `yaml:"work_id,omitempty"`
	InputDir  string   `yaml:"input_dir"`
	Reference string   `yaml:"reference_dir"`
	SHA256    string   `yaml:"input_sha256"`
}

// Matrix describes provider-neutral model/effort routes. A profile can be run
// unchanged with Codex today and Claude later by adding provider-specific profiles.
type Matrix struct {
	Version  int           `yaml:"version"`
	Pricing  pricing.Table `yaml:"pricing"`
	Profiles []Profile     `yaml:"profiles"`
}

type Profile struct {
	ID               string            `yaml:"id"`
	Backend          string            `yaml:"backend"`
	CLIPath          string            `yaml:"cli_path,omitempty"`
	Models           map[string]string `yaml:"models"`
	Efforts          map[string]string `yaml:"efforts,omitempty"`
	MaxAgentsPerBook int               `yaml:"max_agents_per_book"`
	Judges           []Judge           `yaml:"judges,omitempty"`
}

type Judge struct {
	Backend string `yaml:"backend"`
	CLIPath string `yaml:"cli_path,omitempty"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort,omitempty"`
}

var replayAgentStages = []state.State{
	state.SpellingResearch,
	state.FactPass,
	state.Synthesizing,
	state.Auditing,
	state.Fixing,
}

func LoadSuiteSpec(path string) (SuiteSpec, error) {
	var v SuiteSpec
	if err := loadYAML(path, &v); err != nil {
		return v, err
	}
	if v.Version != schemaVersion {
		return v, fmt.Errorf("suite spec version %d, want %d", v.Version, schemaVersion)
	}
	seen := map[string]bool{}
	for i, c := range v.Cases {
		if !idPattern.MatchString(c.ID) {
			return v, fmt.Errorf("cases[%d].id %q is not a safe benchmark id", i, c.ID)
		}
		if seen[c.ID] {
			return v, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.SourceDir) == "" {
			return v, fmt.Errorf("case %q needs title and source_work_dir", c.ID)
		}
	}
	if len(v.Cases) == 0 {
		return v, fmt.Errorf("suite spec has no cases")
	}
	return v, nil
}

func LoadSuite(path string) (Suite, error) {
	var v Suite
	if err := loadYAML(path, &v); err != nil {
		return v, err
	}
	if v.Version != schemaVersion {
		return v, fmt.Errorf("suite version %d, want %d", v.Version, schemaVersion)
	}
	if len(v.Cases) == 0 {
		return v, fmt.Errorf("suite has no cases")
	}
	seen := map[string]bool{}
	for i, c := range v.Cases {
		if !idPattern.MatchString(c.ID) || seen[c.ID] {
			return v, fmt.Errorf("suite case id %q is invalid or duplicated", c.ID)
		}
		seen[c.ID] = true
		if !safeRelative(c.InputDir) || !safeRelative(c.Reference) {
			return v, fmt.Errorf("suite case %q has unsafe input/reference path", c.ID)
		}
		decoded, err := hex.DecodeString(c.SHA256)
		if err != nil || len(decoded) != sha256Size {
			return v, fmt.Errorf("suite cases[%d] has invalid input_sha256", i)
		}
	}
	return v, nil
}

const sha256Size = 32

func safeRelative(path string) bool {
	clean := filepath.Clean(filepath.FromSlash(path))
	return path != "" && !filepath.IsAbs(clean) && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func LoadMatrix(path string) (Matrix, error) {
	var v Matrix
	if err := loadYAML(path, &v); err != nil {
		return v, err
	}
	if v.Version != schemaVersion {
		return v, fmt.Errorf("matrix version %d, want %d", v.Version, schemaVersion)
	}
	seen := map[string]bool{}
	for i := range v.Profiles {
		p := &v.Profiles[i]
		if !idPattern.MatchString(p.ID) || seen[p.ID] {
			return v, fmt.Errorf("profile id %q is invalid or duplicated", p.ID)
		}
		seen[p.ID] = true
		if p.Backend != "codex" && p.Backend != "claude" {
			return v, fmt.Errorf("profile %q backend %q must be codex or claude", p.ID, p.Backend)
		}
		if p.MaxAgentsPerBook < 1 || p.MaxAgentsPerBook > 32 {
			return v, fmt.Errorf("profile %q max_agents_per_book must be 1..32", p.ID)
		}
		allowed := map[string]bool{}
		for _, st := range replayAgentStages {
			allowed[string(st)] = true
		}
		for stage, model := range p.Models {
			if !allowed[stage] || strings.TrimSpace(model) == "" {
				return v, fmt.Errorf("profile %q has invalid model route %q=%q", p.ID, stage, model)
			}
		}
		for stage, effort := range p.Efforts {
			if !allowed[stage] || !validEffort(effort) {
				return v, fmt.Errorf("profile %q has invalid effort route %q=%q", p.ID, stage, effort)
			}
		}
		for _, stage := range replayAgentStages {
			if strings.TrimSpace(p.Models[string(stage)]) == "" {
				return v, fmt.Errorf("profile %q is missing model route %q", p.ID, stage)
			}
		}
		for j, judge := range p.Judges {
			if (judge.Backend != "codex" && judge.Backend != "claude") || strings.TrimSpace(judge.Model) == "" || !validEffort(judge.Effort) {
				return v, fmt.Errorf("profile %q has invalid judge %d", p.ID, j)
			}
		}
		if len(p.Judges) == 0 {
			return v, fmt.Errorf("profile %q needs at least one holdout judge", p.ID)
		}
	}
	if len(v.Profiles) == 0 {
		return v, fmt.Errorf("matrix has no profiles")
	}
	return v, nil
}

func validEffort(v string) bool {
	switch v {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func loadYAML(path string, dst any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // explicit operator-supplied path
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func sortedProfileIDs(m Matrix) []string {
	ids := make([]string, 0, len(m.Profiles))
	for _, p := range m.Profiles {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)
	return ids
}
