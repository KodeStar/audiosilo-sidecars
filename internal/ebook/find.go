package ebook

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/extract"
)

// Candidate is one discovered .epub plus the identity its OPF states.
type Candidate struct {
	// Path is the absolute path to the .epub; Dir is the folder holding it, which
	// is what pairs an epub with an audiobook sitting alongside it.
	Path string
	Dir  string

	Title     string
	Subtitle  string
	Authors   []string
	Language  string
	ISBN      string
	Series    string
	SeriesPos string
	SizeBytes int64

	// MetaErr is set when the OPF could not be read - a DRM-wrapped or corrupt
	// file. Such a candidate is still REPORTED, with a filename-derived title, so
	// the user sees it and learns why it is unusable rather than wondering why a
	// book they own never appeared.
	MetaErr string
}

// maxWalkDepth bounds how deep Find descends below a root. Deep enough for the
// usual author/series/book nesting, shallow enough that a mistakenly-configured
// root (a home directory, a mounted volume) cannot turn a scan into a full-disk
// crawl.
const maxWalkDepth = 8

// Find walks root and returns every .epub beneath it, keyed by absolute path.
//
// It reads each file's container and OPF but never a content document, so it stays
// cheap enough to run across a whole library during a scan. A file it cannot parse
// is reported with MetaErr rather than dropped.
//
// onProgress, when non-nil, is called as directories are walked so a caller can
// stream progress; it may be called from the walking goroutine.
func Find(ctx context.Context, root string, onProgress func(dirs, found int)) (map[string]Candidate, error) {
	out := map[string]Candidate{}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	dirs, found := 0, 0

	err = filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// An unreadable subtree must not fail the whole sweep: one permission
			// error should not cost the user every other book in the library.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != rootAbs && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			if depth(rootAbs, p) > maxWalkDepth {
				return fs.SkipDir
			}
			dirs++
			if onProgress != nil {
				onProgress(dirs, found)
			}
			return nil
		}
		if !IsEpub(d.Name()) {
			return nil
		}
		found++
		out[p] = describe(p, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// describe reads one epub's OPF identity, degrading to a filename-derived title
// when the file cannot be parsed.
func describe(path string, d fs.DirEntry) Candidate {
	c := Candidate{Path: path, Dir: filepath.Dir(path)}
	if info, err := d.Info(); err == nil {
		c.SizeBytes = info.Size()
	}
	md, err := extract.ReadMetadata(path)
	if err != nil {
		c.MetaErr = err.Error()
		c.Title = titleFromFileName(path)
		return c
	}
	c.Title, c.Subtitle = md.Title, md.Subtitle
	c.Authors, c.Language = md.Authors, md.Language
	c.ISBN, c.Series, c.SeriesPos = md.ISBN, md.Series, md.SeriesPos
	if c.Title == "" {
		c.Title = titleFromFileName(path)
	}
	return c
}

// titleFromFileName is the last-resort title for an epub whose OPF states none.
// It only strips the extension - it does not try to parse author or series out of
// a filename, because a guess there becomes a wrong catalogue match.
func titleFromFileName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// depth counts path separators between root and p.
func depth(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

// ByDir groups candidates by their containing folder, which is how an epub is
// paired with an audiobook occupying the same directory.
func ByDir(cands map[string]Candidate) map[string][]Candidate {
	out := map[string][]Candidate{}
	for _, c := range cands {
		out[c.Dir] = append(out[c.Dir], c)
	}
	return out
}
