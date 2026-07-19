package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/fsutil"
	"gopkg.in/yaml.v3"
)

var inputFiles = []string{"manifest.json", "marker_titles.txt"}
var inputDirs = []string{"transcripts-text", "transcripts-repaired"}
var referenceFiles = []string{
	"audit.json", "chunk_plan.json", "corrections.json", "spellings.json", "validation_report.json",
}
var referenceDirs = []string{"facts", "sidecars"}

// Prepare builds a private, self-contained corpus from completed production work
// directories. It never copies source audio, chapter FLACs, raw ASR JSON, run dirs,
// secrets, or databases.
func Prepare(spec SuiteSpec, destination string, now time.Time) (Suite, error) {
	if err := ensureNewDir(destination); err != nil {
		return Suite{}, err
	}
	suite := Suite{Version: schemaVersion, CreatedAt: now.UTC().Format(time.RFC3339)}
	for _, c := range spec.Cases {
		caseRoot := filepath.Join(destination, "cases", c.ID)
		input := filepath.Join(caseRoot, "input")
		ref := filepath.Join(caseRoot, "reference")
		if err := os.MkdirAll(input, 0o750); err != nil {
			return Suite{}, err
		}
		if err := copySelected(c.SourceDir, input, inputFiles, inputDirs, true); err != nil {
			return Suite{}, fmt.Errorf("prepare case %q input: %w", c.ID, err)
		}
		if err := os.MkdirAll(ref, 0o750); err != nil {
			return Suite{}, err
		}
		if err := copySelected(c.SourceDir, ref, referenceFiles, referenceDirs, false); err != nil {
			return Suite{}, fmt.Errorf("prepare case %q reference: %w", c.ID, err)
		}
		digest, err := treeDigest(input)
		if err != nil {
			return Suite{}, err
		}
		suite.Cases = append(suite.Cases, PreparedCase{
			ID: c.ID, Title: c.Title, Authors: c.Authors, Series: c.Series,
			SeriesPos: c.SeriesPos, WorkID: c.WorkID,
			InputDir:  filepath.ToSlash(filepath.Join("cases", c.ID, "input")),
			Reference: filepath.ToSlash(filepath.Join("cases", c.ID, "reference")), SHA256: digest,
		})
	}
	manifest := filepath.Join(destination, "suite.yaml")
	if err := writeYAML(manifest, suite); err != nil {
		return Suite{}, err
	}
	return suite, nil
}

func ensureNewDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("destination is empty")
	}
	if entries, err := os.ReadDir(path); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("destination %s is not empty", path)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(path, 0o750)
}

func copySelected(srcRoot, dstRoot string, files, dirs []string, requireInput bool) error {
	for _, name := range files {
		src := filepath.Join(srcRoot, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) && !requireInput {
				continue
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := fsutil.CopyFile(src, filepath.Join(dstRoot, name), 0o644); err != nil {
			return err
		}
	}
	for _, name := range dirs {
		src := filepath.Join(srcRoot, name)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				if requireInput && name == "transcripts-text" {
					return fmt.Errorf("%s: %w", name, err)
				}
				continue
			}
			return err
		}
		if err := copyTree(src, filepath.Join(dstRoot, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("refusing non-regular corpus entry %s", path)
		}
		return fsutil.CopyFile(path, target, 0o644)
	})
}

func treeDigest(root string) (string, error) {
	var names []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
				return fmt.Errorf("refusing non-regular corpus entry %s", path)
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(h, name+"\x00")
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(name))) //nolint:gosec // contained by walked root
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeYAML(path string, v any) error {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, raw, 0o644)
}
