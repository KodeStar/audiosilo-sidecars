// Package ebook derives a book's logical chapter universe from a split epub.
//
// It is the ebook counterpart of internal/audio's marker normalization, and it
// exists for the same reason: the numbers it produces become the SPOILER
// POSITIONS of the published characters/recaps sidecars, so getting them wrong is
// not a cosmetic error. A chapter numbered too low gates a reveal earlier than the
// book does.
//
// The package is pure - no I/O beyond what the caller passes in, no scheduler or
// store types - so every rule here is table-testable.
package ebook

import (
	"path/filepath"
	"strings"
)

// Work-dir layout for the ebook path.
const (
	// ExtractDir holds the raw pkg/extract output (per-section text plus its own
	// manifest). Scratch: it is the source book's prose and must not outlive the
	// book, so purge reclaims it.
	ExtractDir = "extract"
	// TextDir holds one file per LOGICAL chapter, the layer fact_pass stages and
	// validating n-grams against.
	//
	// It is deliberately not transcripts-corrected/ (the audio path's equivalent):
	// that directory is DURABLE on the audio path - it is the n-gram source and the
	// series-carryover corpus - whereas ebook text is both cheap to regenerate and
	// the copyrighted source prose, so it must be purged.
	TextDir = "ebook-text"
	// ManifestName is the extract-stage audit trail: every emitted section with its
	// label, word count and chapter verdict. The chapter_mapping agent reads it, and
	// so does a human debugging a parked book.
	ManifestName = "extract_manifest.json"
)

// IsEpub reports whether name has the .epub extension.
func IsEpub(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".epub")
}

// ChapterFileName is the per-chapter text file for logical chapter n, matching the
// audio path's chNNN.txt convention so the back half's staging loop is identical.
func ChapterFileName(n int) string {
	return chapterStem(n) + ".txt"
}
