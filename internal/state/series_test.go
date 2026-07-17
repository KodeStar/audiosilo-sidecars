package state

import (
	"math"
	"testing"
)

// TestParseSeriesPos pins the leading-number parse both the scheduler's series lock
// and the ETA queue simulation order books by. This moved here from the scheduler/eta
// duplicates it replaced.
func TestParseSeriesPos(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1", 1},
		{"2.5", 2.5},
		{"1-3.5", 1},  // omnibus range -> leading number
		{" 4 ", 4},    // surrounding whitespace trimmed
		{"10", 10},    // multi-digit
		{"3a", 3},     // trailing non-numeric ignored
		{"", 1e18},    // empty sorts last
		{"foo", 1e18}, // no leading number sorts last
		{"-1", 1e18},  // leading sign is not part of the numeric run -> unparseable
		{".", 1e18},   // a lone dot does not parse
	}
	for _, c := range cases {
		got := ParseSeriesPos(c.in)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ParseSeriesPos(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// An unparseable position must sort AFTER any real position (it is the largest).
	if ParseSeriesPos("") <= ParseSeriesPos("9999") {
		t.Error("unparseable position should sort last (largest)")
	}
}
