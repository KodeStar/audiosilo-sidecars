package pipeline

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kodestar/audiosilo-sidecars/internal/audio"
	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

func TestPlanChunks(t *testing.T) {
	cw := func(pairs ...int) []chapterWords {
		out := make([]chapterWords, 0, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, chapterWords{Chapter: pairs[i], Words: pairs[i+1]})
		}
		return out
	}
	cases := []struct {
		name   string
		in     []chapterWords
		chunks []chunkRange
		ends   []int
	}{
		{
			name:   "whole book under budget is one chunk",
			in:     cw(1, 1000, 2, 1000, 3, 1000),
			chunks: []chunkRange{{From: 1, To: 3}},
			ends:   []int{3},
		},
		{
			name:   "closes each time the budget is reached",
			in:     cw(1, 10000, 2, 15000, 3, 5000, 4, 20000),
			chunks: []chunkRange{{From: 1, To: 2}, {From: 3, To: 4}},
			ends:   []int{2, 4},
		},
		{
			name:   "single chapter over budget is its own chunk",
			in:     cw(1, 30000, 2, 22000),
			chunks: []chunkRange{{From: 1, To: 1}, {From: 2, To: 2}},
			ends:   []int{1, 2},
		},
		{
			name:   "tiny trailing chunk merges into the previous",
			in:     cw(1, 22000, 2, 3000),
			chunks: []chunkRange{{From: 1, To: 2}},
			ends:   []int{2},
		},
		{
			name:   "large trailing chunk is NOT merged",
			in:     cw(1, 22000, 2, 22000),
			chunks: []chunkRange{{From: 1, To: 1}, {From: 2, To: 2}},
			ends:   []int{1, 2},
		},
		{
			name:   "a lone small book stays one chunk (no merge with nothing)",
			in:     cw(1, 500),
			chunks: []chunkRange{{From: 1, To: 1}},
			ends:   []int{1},
		},
		{
			name:   "empty input yields an empty plan",
			in:     nil,
			chunks: nil,
			ends:   []int{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planChunks(tc.in)
			if !reflect.DeepEqual(got.Chunks, tc.chunks) {
				t.Errorf("chunks = %+v, want %+v", got.Chunks, tc.chunks)
			}
			if !reflect.DeepEqual(got.ChunkEnds, tc.ends) {
				t.Errorf("ends = %v, want %v", got.ChunkEnds, tc.ends)
			}
			// ends[i] must always equal chunks[i].To.
			for i, c := range got.Chunks {
				if got.ChunkEnds[i] != c.To {
					t.Errorf("ends[%d]=%d does not match chunk To %d", i, got.ChunkEnds[i], c.To)
				}
			}
		})
	}
}

func TestChapterWordCountPrefersRepaired(t *testing.T) {
	work := t.TempDir()
	if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), 1, "one two three"); err != nil {
		t.Fatal(err)
	}
	// A repaired copy of chapter 1 must win over the raw text.
	if err := transcript.WriteText(filepath.Join(work, transcript.RepairedDir), 1, "one two three four five six"); err != nil {
		t.Fatal(err)
	}
	if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), 2, "alpha beta"); err != nil {
		t.Fatal(err)
	}
	if n, err := chapterWordCount(work, 1); err != nil || n != 6 {
		t.Errorf("chapter 1 words = %d (%v), want 6 (repaired preferred)", n, err)
	}
	if n, err := chapterWordCount(work, 2); err != nil || n != 2 {
		t.Errorf("chapter 2 words = %d (%v), want 2", n, err)
	}
	if n, err := chapterWordCount(work, 3); err != nil || n != 0 {
		t.Errorf("absent chapter 3 words = %d (%v), want 0", n, err)
	}
}

func TestComputeAndRoundTripChunkPlan(t *testing.T) {
	work := t.TempDir()
	// Three tiny chapters -> one chunk under budget.
	m := audio.Manifest{Source: "/x/b.m4b", Style: audio.StyleMarkers, Duration: 30, ChapterCount: 3}
	for i := 1; i <= 3; i++ {
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i, Start: float64(i - 1), End: float64(i), Duration: 1})
		if err := transcript.WriteText(filepath.Join(work, transcript.TextDir), i, "a b c d"); err != nil {
			t.Fatal(err)
		}
	}
	if err := audio.WriteManifest(work, m); err != nil {
		t.Fatal(err)
	}
	plan, err := computeChunkPlan(work)
	if err != nil {
		t.Fatalf("computeChunkPlan: %v", err)
	}
	if len(plan.Chunks) != 1 || plan.Chunks[0].From != 1 || plan.Chunks[0].To != 3 {
		t.Errorf("plan = %+v, want a single [1,3] chunk", plan.Chunks)
	}
	if err := writeChunkPlan(work, plan); err != nil {
		t.Fatalf("writeChunkPlan: %v", err)
	}
	got, err := loadChunkPlan(work)
	if err != nil {
		t.Fatalf("loadChunkPlan: %v", err)
	}
	if !reflect.DeepEqual(got, plan) {
		t.Errorf("round-tripped plan = %+v, want %+v", got, plan)
	}
	if got.chunkEndsCSV() != "3" {
		t.Errorf("chunkEndsCSV = %q, want %q", got.chunkEndsCSV(), "3")
	}
}
