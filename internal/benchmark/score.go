package benchmark

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Quality struct {
	ValidationClean     bool    `json:"validation_clean"`
	ValidationErrors    int     `json:"validation_errors"`
	ValidationWarnings  int     `json:"validation_warnings"`
	CharacterCount      int     `json:"character_count"`
	ReferenceCharacters int     `json:"reference_character_count"`
	CharacterRecall     float64 `json:"character_name_recall"`
	CharacterJaccard    float64 `json:"character_name_jaccard"`
	RecapCount          int     `json:"recap_count"`
	ReferenceRecaps     int     `json:"reference_recap_count"`
	RecapPointJaccard   float64 `json:"recap_point_jaccard"`
}

func Score(workDir, referenceDir string) Quality {
	var q Quality
	var validation struct {
		Clean            bool `json:"clean"`
		Errors, Warnings []string
	}
	if decodeJSON(filepath.Join(workDir, "validation_report.json"), &validation) {
		q.ValidationClean, q.ValidationErrors, q.ValidationWarnings = validation.Clean, len(validation.Errors), len(validation.Warnings)
	}
	candidateChars, candidateRecaps := loadSidecarShape(filepath.Join(workDir, "sidecars"))
	referenceChars, referenceRecaps := loadSidecarShape(filepath.Join(referenceDir, "sidecars"))
	q.CharacterCount, q.ReferenceCharacters = len(candidateChars), len(referenceChars)
	q.CharacterRecall = recall(candidateChars, referenceChars)
	q.CharacterJaccard = jaccard(candidateChars, referenceChars)
	q.RecapCount, q.ReferenceRecaps = len(candidateRecaps), len(referenceRecaps)
	q.RecapPointJaccard = jaccard(candidateRecaps, referenceRecaps)
	return q
}

func loadSidecarShape(dir string) ([]string, []string) {
	var chars struct {
		Characters []struct {
			Name string `json:"name"`
		} `json:"characters"`
	}
	var recaps struct {
		Recaps []struct {
			Through struct {
				Chapter int `json:"chapter"`
			} `json:"through"`
		} `json:"recaps"`
	}
	_ = decodeJSON(filepath.Join(dir, "characters.json"), &chars)
	_ = decodeJSON(filepath.Join(dir, "recaps.json"), &recaps)
	cn := make([]string, 0, len(chars.Characters))
	for _, c := range chars.Characters {
		cn = append(cn, strings.ToLower(strings.TrimSpace(c.Name)))
	}
	rp := make([]string, 0, len(recaps.Recaps))
	for _, r := range recaps.Recaps {
		rp = append(rp, strconv.Itoa(r.Through.Chapter))
	}
	sort.Strings(cn)
	sort.Strings(rp)
	return unique(cn), unique(rp)
}

func decodeJSON(path string, dst any) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // benchmark-owned path
	return err == nil && json.Unmarshal(raw, dst) == nil
}

func recall(candidate, reference []string) float64 {
	if len(reference) == 0 {
		return 0
	}
	return float64(intersection(candidate, reference)) / float64(len(reference))
}

func jaccard(a, b []string) float64 {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	for _, x := range b {
		set[x] = true
	}
	if len(set) == 0 {
		return 1
	}
	return float64(intersection(a, b)) / float64(len(set))
}

func intersection(a, b []string) int {
	set := map[string]bool{}
	for _, x := range a {
		set[x] = true
	}
	n := 0
	seen := map[string]bool{}
	for _, x := range b {
		if set[x] && !seen[x] {
			n++
			seen[x] = true
		}
	}
	return n
}

func unique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := []string{in[0]}
	for _, x := range in[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
