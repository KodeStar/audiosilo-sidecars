package state

import (
	"sort"
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

// SeriesQueueItem is the minimal book identity needed to assign breadth-first
// scheduling waves across series.
type SeriesQueueItem struct {
	ID        int64
	Series    string
	SeriesPos string
}

// SeriesBreadthRanks assigns each named-series book its zero-based ordinal within
// that series (ordered by numeric position, then id). Seriesless books each receive
// rank zero because they are independent. Sorting work by rank then id yields the
// useful queue shape: the first book from every series, then the second from every
// series, rather than exhausting one series before opening an agent slot for another.
func SeriesBreadthRanks(items []SeriesQueueItem) map[int64]int {
	ranks := make(map[int64]int, len(items))
	groups := map[string][]SeriesQueueItem{}
	for _, item := range items {
		series := strings.TrimSpace(item.Series)
		if series == "" {
			ranks[item.ID] = 0
			continue
		}
		groups[series] = append(groups[series], item)
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			pi, pj := ParseSeriesPos(group[i].SeriesPos), ParseSeriesPos(group[j].SeriesPos)
			if pi != pj {
				return pi < pj
			}
			return group[i].ID < group[j].ID
		})
		for rank, item := range group {
			ranks[item.ID] = rank
		}
	}
	return ranks
}
