package spelling

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-sidecars/internal/transcript"
)

// CandidatesFile is the mechanically-extracted proper-noun candidate report that
// the spelling_research agent reads INSTEAD of the full transcript. It is a compact
// (well under ~150KB for a novel) deterministic distillation of the likely proper
// nouns / invented terms in the corrected/repaired transcript layer, so the agent
// no longer needs the whole ~600KB text staged into its context.
const CandidatesFile = "spelling_candidates.json"

// Candidate-extraction bounds. These keep the output compact (the staged JSON stays
// well under ~150KB even for a dense novel) and deterministic; a drop is never silent
// (Candidates.Truncated records it).
const (
	// minNonInitialSingle is the non-sentence-initial occurrence floor for a single
	// capitalized token to count as a candidate.
	minNonInitialSingle = 2
	// minInitialNovelSingle is the total-occurrence floor for a single capitalized
	// token that is NOT a common English word to count as a candidate even with zero
	// non-initial hits. It rescues real names the sentence-initial gate would drop:
	// a name always preceded by an honorific period ("Mr. Ashford" - the '.' falsely
	// marks Ashford sentence-initial), one always dialogue-addressed at a sentence
	// start ("Kael, wait."), or one in a status-sheet ("Name. Hysteria. High Elf.").
	minInitialNovelSingle = 3
	// minRareLowercase is the occurrence floor for a rare non-dictionary lowercase token.
	minRareLowercase = 3
	// maxPhraseCandidates / maxSingleCandidates cap the two output buckets separately, so
	// a term-dense book's single tokens cannot crowd out the high-signal phrases.
	maxPhraseCandidates = 500
	maxSingleCandidates = 300
	// maxLowercasePhraseLen bounds the lowercase-phrase variant window (words).
	maxLowercasePhraseLen = 3
	// maxSnippets is the number of context snippets per candidate. Snippets are the
	// agent's ONLY textual evidence for a candidate's type/role and its do-not-merge
	// decisions (the full transcript is not staged), so two - each from a DISTINCT
	// chapter - are worth the size; buildSnippets keeps at most one per chapter.
	maxSnippets = 2
	// maxOccStored caps the occurrence positions retained per form for snippet selection.
	maxOccStored = 24
	// snippetBefore / snippetAfter are the word windows around a match (~11 words total),
	// sliced from the original text so the snippet keeps its punctuation.
	snippetBefore = 4
	snippetAfter  = 6
	// maxSnippetRunes clamps a single snippet's length.
	maxSnippetRunes = 120
)

// Snippet is one short context excerpt for a candidate: the chapter it came from and
// ~11 words of surrounding text, sliced from the original transcript (punctuation and
// quotes preserved).
type Snippet struct {
	Chapter int    `json:"chapter"`
	Text    string `json:"text"`
}

// Candidate is one likely proper noun / invented term the agent should verify.
type Candidate struct {
	// Form is the exact surface form ("Leafs Crossing", "night blades", "d'Aston").
	Form string `json:"form"`
	// Count is the total occurrences of Form across the book.
	Count int `json:"count"`
	// NonInitial is the count at non-sentence-initial positions (single capitalized
	// tokens); it equals Count for phrases and lowercase forms.
	NonInitial int `json:"non_initial"`
	// Chapters are the sorted, unique logical-chapter numbers Form appears in.
	Chapters []int `json:"chapters"`
	// Snippets are up to maxSnippets context excerpts, from distinct chapters where possible.
	Snippets []Snippet `json:"snippets"`
}

// Candidates is the spelling_candidates.json contract handed to the agent.
type Candidates struct {
	// GeneratedFrom names the transcript layers the candidates were extracted from.
	GeneratedFrom string `json:"generated_from"`
	// Candidates is the ordered candidate list (count desc, then form asc).
	Candidates []Candidate `json:"candidates"`
	// Truncated is the number of candidates dropped by the output caps (0 = none).
	Truncated int `json:"truncated"`
	// TotalWords is the total token count across the transcript layer the candidates
	// were extracted from. It lets the caller distinguish a genuinely tiny book from a
	// broken extraction (zero candidates over a large corpus - see the stage's floor).
	TotalWords int `json:"total_words"`
}

// tok is one extracted token: its exact surface form, whether it is capitalized
// (first letter uppercase OR an internal apostrophe followed by an uppercase letter,
// so "d'Aston" is capitalized), and whether it is sentence-initial.
type tok struct {
	form    string
	capital bool
	initial bool
	// joinable reports whether the gap between this token and the PREVIOUS token
	// consists solely of ASCII spaces (' '). A comma, semicolon, colon, quote, paren
	// or any other separator - and the first token of a chapter - is NOT joinable, so
	// a phrase run never bridges punctuation ("Nightshade, Deathflower" is a list of
	// two names, not one phrase). See tokenize.
	joinable bool
	// startByte/endByte are the token's byte offsets in the (apostrophe-normalized)
	// chapter text, so a snippet renders a slice of the ORIGINAL text (preserving the
	// commas and quotes a space-join of token forms would hide). See snippetText.
	startByte int
	endByte   int
}

// commonWords is the case-folded set of common English words built once from the
// generated commonWordsRaw list, used by ExtractCandidates category 4 to skip
// ordinary English words.
var commonWords = buildCommonWords()

func buildCommonWords() map[string]bool {
	fields := strings.Fields(commonWordsRaw)
	m := make(map[string]bool, len(fields))
	for _, w := range fields {
		m[strings.ToLower(w)] = true
	}
	return m
}

// chapTokens is one chapter's ordered tokens plus its logical-chapter number.
type chapTokens struct {
	num  int
	toks []tok
	// text is the apostrophe-normalized chapter text the toks index into, kept so a
	// snippet renders the ORIGINAL surrounding text (with punctuation) rather than a
	// space-join of token forms.
	text string
}

// occ is one occurrence of a form: the chapter slice index, the starting token
// index within that chapter, and how many tokens the form spans (1 for a single
// token, >=2 for a phrase).
type occ struct {
	chapIdx int
	tokIdx  int
	span    int
}

// formStat accumulates a form's occurrence statistics across the book.
type formStat struct {
	count      int
	nonInitial int
	capital    bool
	chapters   map[int]bool
	occ        []occ
}

func (fs *formStat) record(chapNum, chapIdx, tokIdx, span int, initial, capital bool) {
	fs.count++
	if !initial {
		fs.nonInitial++
	}
	fs.capital = capital
	if fs.chapters == nil {
		fs.chapters = map[int]bool{}
	}
	fs.chapters[chapNum] = true
	if len(fs.occ) < maxOccStored {
		fs.occ = append(fs.occ, occ{chapIdx: chapIdx, tokIdx: tokIdx, span: span})
	}
}

// ExtractCandidates reads each chapter of a book's transcript (driven by the
// transcripts-text/ filenames, but preferring the transcripts-repaired/ copy per
// chapter exactly like Apply) and produces a deterministic report of the likely
// proper nouns and invented terms in it. It never writes anything; the caller (the
// spelling_research stage) serializes the result into the staged agent dir.
//
// The four heuristics (documented on the JSON contract): multi-word capitalized
// phrases, non-sentence-initial single capitalized tokens, lowercase variants of an
// included capitalized candidate, and rare repeated non-dictionary lowercase tokens.
func ExtractCandidates(workDir string) (*Candidates, error) {
	names, err := listChapterTxt(filepath.Join(workDir, transcript.TextDir))
	if err != nil {
		return nil, err
	}
	chaps := make([]chapTokens, 0, len(names))
	usedRepaired := false
	totalWords := 0
	for _, n := range names {
		num, ok := transcript.ParseChapter(n)
		if !ok {
			continue
		}
		// Prefer the repaired layer over the base text, per chapter; transcript owns
		// that preference (the same one Apply and the chunk planner share).
		src, ok := transcript.ChapterTextPath(workDir, num)
		if !ok {
			continue
		}
		if filepath.Base(filepath.Dir(src)) == transcript.RepairedDir {
			usedRepaired = true // at least one chapter resolved to the repaired layer
		}
		b, rerr := os.ReadFile(src) //nolint:gosec // path derives from the book's work dir
		if rerr != nil {
			return nil, rerr
		}
		normText := normalizeApostrophes(string(b))
		toks := tokenize(normText)
		totalWords += len(toks)
		chaps = append(chaps, chapTokens{num: num, toks: toks, text: normText})
	}

	singles := map[string]*formStat{}
	phrases := map[string]*formStat{}

	for ci := range chaps {
		toks := chaps[ci].toks
		num := chaps[ci].num
		for ti := range toks {
			t := toks[ti]
			recordForm(singles, t.form, num, ci, ti, 1, t.initial, t.capital)
		}
		// Capitalized phrase runs: maximal sequences of consecutive capitalized tokens
		// not split by a sentence boundary (a token that starts a new sentence begins a
		// new run). Emit each run's adjacent bigrams, plus the full run when >= 3 words.
		for _, run := range capitalRuns(toks) {
			emitPhrases(phrases, num, ci, toks, run)
		}
	}

	// Included capitalized candidates: single tokens with enough non-initial hits, and
	// every phrase (high signal - kept even at count 1).
	includedCapSingleCF := map[string]bool{}
	includedPhraseCF := map[string]bool{}
	final := map[string]*Candidate{}

	for form, st := range singles {
		if !st.capital {
			continue
		}
		// A capitalized single is included when it has enough non-initial hits, OR when
		// it is a novel (non-common) word repeated at least minInitialNovelSingle times
		// - the latter rescues a real name the sentence-initial gate keeps zeroing out
		// (an honorific-prefixed, dialogue-addressed, or status-sheet name).
		novelInitial := st.count >= minInitialNovelSingle && !commonWords[strings.ToLower(form)]
		if st.nonInitial >= minNonInitialSingle || novelInitial {
			includedCapSingleCF[strings.ToLower(form)] = true
			final[form] = candidateFrom(form, st, chaps, st.nonInitial)
		}
	}
	for form, st := range phrases {
		includedPhraseCF[strings.ToLower(form)] = true
		final[form] = candidateFrom(form, st, chaps, st.count)
	}

	// Lowercase single-token variants of an included capitalized single, plus rare
	// repeated non-dictionary lowercase tokens.
	for form, st := range singles {
		if st.capital {
			continue
		}
		cf := strings.ToLower(form)
		lowerVariant := includedCapSingleCF[cf]
		rare := st.count >= minRareLowercase && !commonWords[cf]
		if (lowerVariant || rare) && final[form] == nil {
			final[form] = candidateFrom(form, st, chaps, st.count)
		}
	}

	// Lowercase phrase variants: windows of consecutive lowercase tokens whose
	// space-joined casefold matches an included capitalized phrase, OR whose
	// space-removed casefold matches an included capitalized single (an ASR split of
	// a compound term, e.g. "night blades" for "Nightblades").
	for form, st := range lowercasePhraseStats(chaps, includedPhraseCF, includedCapSingleCF) {
		if final[form] == nil {
			final[form] = candidateFrom(form, st, chaps, st.count)
		}
	}

	return assemble(final, usedRepaired, totalWords), nil
}

// recordForm gets or creates the formStat for key in m and records one occurrence
// of a form spanning span tokens at (chapNum, chapIdx, tokIdx). It is the single
// get-or-create idiom shared by the single-token pass, the phrase emitter, and the
// lowercase-phrase scan.
func recordForm(m map[string]*formStat, key string, chapNum, chapIdx, tokIdx, span int, initial, capital bool) {
	st := m[key]
	if st == nil {
		st = &formStat{}
		m[key] = st
	}
	st.record(chapNum, chapIdx, tokIdx, span, initial, capital)
}

// tokenize splits text into tokens. A token is a maximal run of Unicode letters and
// word-internal apostrophes (an apostrophe with a letter on both sides, so "d'Aston"
// and "don't" are single tokens but a leading/trailing/possessive-plural apostrophe
// is a boundary). It iterates over RUNES so a multi-byte name ("Renée") is one token
// rather than shattering into garbage the ASR spelling could never surface. It flags
// each token capitalized and sentence-initial, treating every non-letter as a
// boundary (never a \b that would split inside an apostrophe). It also records each
// token's byte span in text (for original-text snippets) and whether it is joinable
// to the previous token (the inter-token gap was all spaces - the phrase-run gate).
// Text is expected apostrophe-normalized already (the caller runs normalizeApostrophes).
func tokenize(text string) []tok {
	var out []tok
	runes := []rune(text)
	n := len(runes)
	// byteOff[k] is the byte offset of runes[k] in text; byteOff[n] is len(text). It
	// lets each token record its byte span so a snippet slices the ORIGINAL text.
	byteOff := make([]int, n+1)
	b := 0
	for k, r := range runes {
		byteOff[k] = b
		b += utf8.RuneLen(r)
	}
	byteOff[n] = b

	sentenceInitial := true // the first token of a chapter starts a sentence
	firstToken := true      // the first token has no previous token to join to
	gapAllSpaces := true    // whether every non-letter since the previous token is ' '
	i := 0
	for i < n {
		c := runes[i]
		if !unicode.IsLetter(c) {
			// Track sentence boundaries in the inter-token gap.
			if c == '.' || c == '!' || c == '?' || c == '\n' {
				sentenceInitial = true
			}
			// Any non-space in the gap breaks phrase joinability (a comma, quote, paren,
			// semicolon, newline...); only a run of literal ASCII spaces stays joinable.
			if c != ' ' {
				gapAllSpaces = false
			}
			i++
			continue
		}
		// Start of a token: consume letters and word-internal apostrophes.
		start := i
		for i < n {
			if unicode.IsLetter(runes[i]) {
				i++
				continue
			}
			// A word-internal apostrophe: letter before (guaranteed) and letter after.
			if runes[i] == '\'' && i+1 < n && unicode.IsLetter(runes[i+1]) {
				i++
				continue
			}
			break
		}
		form := string(runes[start:i])
		out = append(out, tok{
			form:      form,
			capital:   isCapitalized(form),
			initial:   sentenceInitial,
			joinable:  !firstToken && gapAllSpaces,
			startByte: byteOff[start],
			endByte:   byteOff[i],
		})
		sentenceInitial = false
		firstToken = false
		gapAllSpaces = true // reset: measure the gap before the NEXT token afresh
	}
	return out
}

// isCapitalized reports whether a token's surface form is capitalized: its first
// letter is an uppercase Unicode letter (so "Renée" counts), OR an internal
// apostrophe is immediately followed by an uppercase letter (the "d'Aston" shape).
func isCapitalized(form string) bool {
	runes := []rune(form)
	if len(runes) == 0 {
		return false
	}
	if unicode.IsUpper(runes[0]) {
		return true
	}
	for i := 0; i+1 < len(runes); i++ {
		if runes[i] == '\'' && unicode.IsUpper(runes[i+1]) {
			return true
		}
	}
	return false
}

// capitalRuns returns the [start,end) token index ranges of maximal runs of
// consecutive capitalized tokens, where a token that begins a new sentence starts a
// new run (so a name ending one sentence never joins a name starting the next), and a
// token separated from its predecessor by anything other than spaces (a comma etc.) is
// NOT joined (so "Nightshade, Deathflower" never forms a phrase). A leading token that
// is a sentence-opening common word ("The", "But", "Beyond") is trimmed off the run -
// it is capitalized only because it starts a sentence, so it would otherwise chain into
// a following proper noun as a junk phrase ("The Night").
func capitalRuns(toks []tok) [][2]int {
	var runs [][2]int
	i := 0
	for i < len(toks) {
		if !toks[i].capital {
			i++
			continue
		}
		start := i
		i++
		for i < len(toks) && toks[i].capital && !toks[i].initial && toks[i].joinable {
			i++
		}
		s := start
		if toks[s].initial && commonWords[strings.ToLower(toks[s].form)] {
			s++
		}
		if i-s >= 2 {
			runs = append(runs, [2]int{s, i})
		}
	}
	return runs
}

// emitPhrases records the phrase forms for one capitalized run [start,end): every
// adjacent bigram, plus the full run when it spans exactly 3 tokens (a trigram
// name). Runs of 4+ tokens are almost always stat-screen fragments, not names, so
// only their constituent bigrams are kept (the whole run is not emitted).
func emitPhrases(phrases map[string]*formStat, chapNum, chapIdx int, toks []tok, run [2]int) {
	start, end := run[0], run[1]
	for i := start; i+1 < end; i++ {
		recordForm(phrases, toks[i].form+" "+toks[i+1].form, chapNum, chapIdx, i, 2, false, true)
	}
	if end-start == 3 {
		recordForm(phrases, phraseForm(toks, start, end), chapNum, chapIdx, start, 3, false, true)
	}
}

// phraseForm joins the surface forms of toks[start:end] with single spaces.
func phraseForm(toks []tok, start, end int) string {
	parts := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		parts = append(parts, toks[i].form)
	}
	return strings.Join(parts, " ")
}

// lowercasePhraseStats scans every chapter for windows of 2..maxLowercasePhraseLen
// consecutive ALL-lowercase (non-capitalized) tokens and records those whose
// space-joined casefold is an included capitalized phrase, or whose space-removed
// casefold is an included capitalized single (an ASR split of a compound term). It
// returns a map keyed by the exact lowercase surface form.
func lowercasePhraseStats(chaps []chapTokens, phraseCF, singleCF map[string]bool) map[string]*formStat {
	out := map[string]*formStat{}
	for ci := range chaps {
		toks := chaps[ci].toks
		// Lowercase each token ONCE per chapter, so the overlapping windows below reuse
		// it instead of re-lowering every token in every window.
		lc := make([]string, len(toks))
		for i := range toks {
			lc[i] = strings.ToLower(toks[i].form)
		}
		for ti := range toks {
			if toks[ti].capital {
				continue // an all-lowercase window cannot start on a capitalized token
			}
			var joined, stripped strings.Builder
			joined.WriteString(lc[ti])
			stripped.WriteString(lc[ti])
			for l := 2; l <= maxLowercasePhraseLen && ti+l <= len(toks); l++ {
				next := ti + l - 1
				if toks[next].capital {
					break // the newly appended token is capitalized: the window ends here
				}
				if !toks[next].joinable {
					break // a comma/quote separates it from the prior token: the phrase ends
				}
				joined.WriteByte(' ')
				joined.WriteString(lc[next])
				stripped.WriteString(lc[next])
				// Probe both included-form maps directly (they are tiny): a space-removed
				// compound match (an ASR split of "Nightblades") or a space-joined phrase
				// match ("night blades" for "Night Blades").
				if !singleCF[stripped.String()] && !phraseCF[joined.String()] {
					continue
				}
				recordForm(out, phraseForm(toks, ti, ti+l), chaps[ci].num, ci, ti, l, false, false)
			}
		}
	}
	return out
}

// candidateFrom builds a Candidate from a form's stat, choosing up to maxSnippets
// snippets preferring distinct chapters.
func candidateFrom(form string, st *formStat, chaps []chapTokens, nonInitial int) *Candidate {
	chapters := make([]int, 0, len(st.chapters))
	for c := range st.chapters {
		chapters = append(chapters, c)
	}
	sort.Ints(chapters)
	return &Candidate{
		Form:       form,
		Count:      st.count,
		NonInitial: nonInitial,
		Chapters:   chapters,
		Snippets:   buildSnippets(st.occ, chaps),
	}
}

// buildSnippets picks up to maxSnippets occurrences, ONE per distinct chapter, and
// renders each as ~11 words of surrounding context. At most one snippet per chapter is
// intentional (no same-chapter backfill): distinct chapters spread the spoiler
// evidence wider than repeat hits within a single chapter would, so a candidate
// confined to one chapter yields exactly one snippet.
func buildSnippets(occs []occ, chaps []chapTokens) []Snippet {
	if len(occs) == 0 {
		return nil
	}
	seen := map[int]bool{}
	var picked []occ
	for _, o := range occs {
		if len(picked) >= maxSnippets {
			break
		}
		num := chaps[o.chapIdx].num
		if seen[num] {
			continue
		}
		seen[num] = true
		picked = append(picked, o)
	}
	out := make([]Snippet, 0, len(picked))
	for _, o := range picked {
		out = append(out, Snippet{Chapter: chaps[o.chapIdx].num, Text: snippetText(chaps[o.chapIdx], o)})
	}
	return out
}

// snippetText renders ~11 words of context around an occurrence by slicing the
// ORIGINAL (apostrophe-normalized) chapter text from ~snippetBefore words before the
// occurrence's first token to ~snippetAfter words after its last token. Slicing the
// real text (rather than joining token forms) keeps the punctuation - commas, quotes -
// the agent needs to see a comma-separated list is NOT a phrase. Clamped in length.
func snippetText(ct chapTokens, o occ) string {
	toks := ct.toks
	if len(toks) == 0 || o.tokIdx >= len(toks) {
		return ""
	}
	lo := max(o.tokIdx-snippetBefore, 0)
	hi := min(o.tokIdx+o.span+snippetAfter, len(toks))
	if lo >= hi {
		return ""
	}
	s := ct.text[toks[lo].startByte:toks[hi-1].endByte]
	if r := []rune(s); len(r) > maxSnippetRunes {
		s = strings.TrimRight(string(r[:maxSnippetRunes]), " ") + "..."
	}
	return s
}

// assemble splits the candidates into phrases (multi-word) and single tokens, caps
// each bucket separately (phrases get their own budget so single tokens cannot crowd
// out low-count named phrases), then emits the union sorted by count desc, then form
// asc. Any drop in either bucket is recorded in Truncated (never silent).
func assemble(final map[string]*Candidate, usedRepaired bool, totalWords int) *Candidates {
	var phrases, singles []Candidate
	for _, c := range final {
		if strings.Contains(c.Form, " ") {
			phrases = append(phrases, *c)
		} else {
			singles = append(singles, *c)
		}
	}
	sortCandidates(phrases)
	// Single tokens are capped non-dictionary-FIRST: an invented/misheard term (not a
	// common English word) is exactly what the spelling stage exists to fix, whereas a
	// frequent ordinary word that merely gets capitalized is low orthographic risk. So
	// rank the single bucket by (non-dictionary, count, form) for the cap, which keeps
	// the invented names ("Nightblades", "Sanren") ahead of common capitalized words.
	// Precompute each single's novelty ONCE (one commonWords lookup per form) rather
	// than re-deriving it inside the O(n log n) comparator.
	novel := make(map[string]bool, len(singles))
	for _, c := range singles {
		novel[c.Form] = singleIsNovel(c.Form)
	}
	sort.Slice(singles, func(i, j int) bool {
		if novel[singles[i].Form] != novel[singles[j].Form] {
			return novel[singles[i].Form]
		}
		if singles[i].Count != singles[j].Count {
			return singles[i].Count > singles[j].Count
		}
		return singles[i].Form < singles[j].Form
	})

	truncated := 0
	if len(phrases) > maxPhraseCandidates {
		truncated += len(phrases) - maxPhraseCandidates
		phrases = phrases[:maxPhraseCandidates]
	}
	if len(singles) > maxSingleCandidates {
		truncated += len(singles) - maxSingleCandidates
		singles = singles[:maxSingleCandidates]
	}

	cands := make([]Candidate, 0, len(phrases)+len(singles))
	cands = append(cands, phrases...)
	cands = append(cands, singles...)
	sortCandidates(cands)
	// Report only the layers actually read: name the repaired layer only when at least
	// one chapter resolved to it (otherwise "transcripts-text" is the honest source).
	generatedFrom := transcript.TextDir
	if usedRepaired {
		generatedFrom = transcript.TextDir + " + " + transcript.RepairedDir
	}
	return &Candidates{
		GeneratedFrom: generatedFrom,
		Candidates:    cands,
		Truncated:     truncated,
		TotalWords:    totalWords,
	}
}

// singleIsNovel reports whether a single-token form is NOT a common English word
// (case-folded) - i.e. a likely invented/misheard proper noun rather than an
// ordinary word that happens to be capitalized.
func singleIsNovel(form string) bool {
	return !commonWords[strings.ToLower(form)]
}

// sortCandidates orders by count desc, then form asc (deterministic).
func sortCandidates(cands []Candidate) {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Count != cands[j].Count {
			return cands[i].Count > cands[j].Count
		}
		return cands[i].Form < cands[j].Form
	})
}

// MarshalCandidates serializes a Candidates report as COMPACT JSON with a trailing
// newline (the on-disk form the stage stages for the agent). Compact, not indented:
// a dense novel's candidate arrays (chapters, snippets) would triple in size under
// MarshalIndent, and the agent reads either form equally well.
func MarshalCandidates(c *Candidates) ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
