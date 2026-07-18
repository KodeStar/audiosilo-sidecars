package qa

// FilterReport returns a chapter-bounded copy suitable for an independent QA
// adjudication worker. Book-wide summary statistics remain informational, while
// every finding and required disposition is restricted to chapters in keep.
func FilterReport(r *Report, chapters []int) *Report {
	keep := make(map[int]bool, len(chapters))
	for _, ch := range chapters {
		keep[ch] = true
	}
	out := &Report{Chapters: r.Chapters, WPHMean: r.WPHMean, WPHStdDev: r.WPHStdDev,
		WPHOutliers: []WPHOutlier{}, RepeatedRuns: []RepeatedRun{}, CrossSegment: []CrossSegmentHit{}, WithinSegment: []WithinSegmentHit{}, MultiLoop: []MultiLoopFinding{}, TailRate: []TailRateHit{}, RetranscribeQueue: []int{},
		LowConfidence: LowConfidence{TotalLow: r.LowConfidence.TotalLow, TotalWords: r.LowConfidence.TotalWords, Worst: []LowConfChapter{}}}
	for _, v := range r.WPHOutliers {
		if keep[v.Chapter] {
			out.WPHOutliers = append(out.WPHOutliers, v)
		}
	}
	for _, v := range r.RepeatedRuns {
		if keep[v.Chapter] {
			out.RepeatedRuns = append(out.RepeatedRuns, v)
		}
	}
	for _, v := range r.LowConfidence.Worst {
		if keep[v.Chapter] {
			out.LowConfidence.Worst = append(out.LowConfidence.Worst, v)
		}
	}
	for _, v := range r.CrossSegment {
		if keep[v.Chapter] {
			out.CrossSegment = append(out.CrossSegment, v)
		}
	}
	for _, v := range r.WithinSegment {
		if keep[v.Chapter] {
			out.WithinSegment = append(out.WithinSegment, v)
		}
	}
	for _, v := range r.MultiLoop {
		if keep[v.Chapter] {
			out.MultiLoop = append(out.MultiLoop, v)
		}
	}
	for _, v := range r.TailRate {
		if keep[v.Chapter] {
			out.TailRate = append(out.TailRate, v)
		}
	}
	for _, v := range r.RetranscribeQueue {
		if keep[v] {
			out.RetranscribeQueue = append(out.RetranscribeQueue, v)
		}
	}
	return out
}
