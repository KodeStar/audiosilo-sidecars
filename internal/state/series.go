package state

import (
	"strconv"
	"strings"
)

// unparsablePos is the sort key for a position that carries no leading number: it
// sorts last so an unlabelled book never displaces a numbered predecessor.
const unparsablePos = 1e18

// ParseSeriesPos extracts the leading number of a series-position string ("1",
// "2.5", "1-3.5" -> 1). An empty or unparseable position sorts last (unparsablePos).
// It lives in this pure leaf package because the scheduler's series lock, the ETA
// queue simulation, and the pipeline's series-carryover discovery all order books by
// position and all call it directly, and none may import the others.
func ParseSeriesPos(pos string) float64 {
	pos = strings.TrimSpace(pos)
	if pos == "" {
		return unparsablePos
	}
	end := 0
	for end < len(pos) {
		c := pos[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
			continue
		}
		break
	}
	f, err := strconv.ParseFloat(pos[:end], 64)
	if err != nil {
		return unparsablePos
	}
	return f
}
