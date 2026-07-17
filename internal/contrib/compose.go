package contrib

import (
	"errors"
	"strconv"
	"strings"
)

// BodySizeLimit is the composed-body byte threshold above which the JSON payload
// is uploaded as a gist and linked, rather than inlined. GitHub caps an issue
// body at 65536 chars; recaps for a very long book can approach that, so we
// switch to a gist link well before the hard limit.
const BodySizeLimit = 60000

// ExceedsBodyLimit reports whether a composed body is large enough that the
// stage should re-compose it with a gist link instead of an inline payload.
func ExceedsBodyLimit(body string) bool {
	return len(body) > BodySizeLimit
}

// Routing labels (the meta repo's intake workflow routes on these, not on the
// title). They must match .github/ISSUE_TEMPLATE/*.yml exactly.
var (
	charactersLabels = []string{"data", "data:characters"}
	recapsLabels     = []string{"data", "data:recaps"}
	workLabels       = []string{"data", "data:add-work"}
)

// Field heading labels. These are the `### <label>` headings metaissue's parser
// (internal/issueform/parse.go + compose_sidecar.go/compose_work.go) reads
// VERBATIM, so they are copied from the parser constants and the form YAML.
const (
	hWork             = "The work"
	hCharactersFile   = "The characters.json file"
	hRecapsFile       = "The recaps.json file"
	hOwnWords         = "Own words"
	hNeutralVoice     = "Neutral voice"
	hLicense          = "License"
	hTitle            = "Title"
	hSubtitle         = "Subtitle"
	hAuthors          = "Author(s)"
	hLanguage         = "Language"
	hFirstPublished   = "First published (year)"
	hSeriesName       = "Series name"
	hSeriesPosition   = "Series position"
	hISBN             = "ISBN(s)"
	hNarrators        = "Narrator(s)"
	hAbridged         = "Abridged?"
	hRuntime          = "Runtime (minutes)"
	hReleaseDate      = "Release date"
	hPublisher        = "Publisher"
	hASINs            = "ASIN(s) with region"
	hAudiobookISBNs   = "Audiobook ISBN(s)"
	hCoverURL         = "Cover image URL"
	hSources          = "Sources"
	hFactualData      = "Factual data"
	hPublicDedication = "Public domain dedication"
)

// Checkbox item texts. These must match the form YAML's `options[].label`
// verbatim (metaissue only checks the box is ticked, but rendering the real text
// keeps the composed body a faithful reproduction of a submitted form).
const (
	itemOwnWordsCharacters = "Every description is my own words - no verbatim or near-verbatim text from the book, its jacket copy, or any wiki."
	itemNeutralCharacters  = "The entries use a neutral reference-guide voice - no jokes, editorializing, profanity, or value judgements."
	itemOwnWordsRecaps     = "Every recap is my own words - no verbatim or near-verbatim text from the book, its jacket copy, or any wiki - and stays within the length caps."
	itemNeutralRecaps      = "The recaps use a neutral reference-guide voice - no jokes, editorializing, profanity, or value judgements - and the final entry states the actual ending."
	itemCCBySALicense      = "I license this contribution under CC BY-SA 3.0, and I have the right to do so."
	itemFactualData        = "This submission is factual data (no publisher blurb or copyrighted description)."
	itemCC0Dedication      = "I dedicate this contribution to the public domain under CC0-1.0, and I have the right to do so."
)

// RegionASIN is one region-scoped ASIN, rendered as a "region: ASIN" line in the
// add-work form's ASIN textarea. The JSON tags are the wire contract the web core
// proposal form (Wave 3B) reads/writes.
type RegionASIN struct {
	Region string `json:"region"`
	ASIN   string `json:"asin"`
}

// CoreProposal is the data for an add-work intake issue: the abstract work plus
// its first recording. Validate() enforces metaissue's required fields; optional
// fields render only when non-empty. The snake_case JSON tags are the wire
// contract: the pipeline writes contrib/core_proposal.json in this shape, the API
// serves it on GET /books/{id}/contrib/core, and POST /books/{id}/contribute/core
// accepts it back.
type CoreProposal struct {
	Title          string       `json:"title"`
	Subtitle       string       `json:"subtitle,omitempty"`
	Authors        []string     `json:"authors"`
	Language       string       `json:"language"`
	FirstPublished string       `json:"first_published,omitempty"`
	SeriesName     string       `json:"series_name,omitempty"`
	SeriesPosition string       `json:"series_position,omitempty"`
	PrintISBNs     []string     `json:"print_isbns,omitempty"`
	Narrators      []string     `json:"narrators"`
	Abridged       string       `json:"abridged,omitempty"` // "" | "Unabridged" | "Abridged"
	RuntimeMin     int          `json:"runtime_min,omitempty"`
	ReleaseDate    string       `json:"release_date,omitempty"`
	Publisher      string       `json:"publisher,omitempty"`
	ASINs          []RegionASIN `json:"asins,omitempty"`
	AudiobookISBNs []string     `json:"audiobook_isbns,omitempty"`
	CoverURL       string       `json:"cover_url,omitempty"`
	Sources        string       `json:"sources"`
}

// Validate enforces the fields metaissue's add-work composer requires (Title,
// at least one Author, a Language, at least one Narrator, and Sources). The
// error names the missing field so the API can surface a 400 message.
func (p CoreProposal) Validate() error {
	if strings.TrimSpace(p.Title) == "" {
		return errors.New("title is required")
	}
	if len(nonEmpty(p.Authors)) == 0 {
		return errors.New("at least one author is required")
	}
	if strings.TrimSpace(p.Language) == "" {
		return errors.New("language is required")
	}
	if len(nonEmpty(p.Narrators)) == 0 {
		return errors.New("at least one narrator is required")
	}
	if strings.TrimSpace(p.Sources) == "" {
		return errors.New("sources is required")
	}
	// An ASIN needs a region to be region-scoped (the prefill leaves it blank for the
	// user to pick). A fully-empty entry is fine - asinLines filters it out at render.
	for _, a := range p.ASINs {
		if strings.TrimSpace(a.ASIN) != "" && strings.TrimSpace(a.Region) == "" {
			return errors.New("each ASIN needs a region - pick a region for the ASIN")
		}
	}
	return nil
}

// sidecarIssue composes the shared characters/recaps intake-issue shape: the work
// field, the attachment section, the own-words / neutral-voice / license checkboxes,
// and the "[kind] <slug>" title. CharactersIssue and RecapsIssue differ only in the
// per-dimension parameters passed here; the composed bytes MUST stay byte-identical to
// the hand-written form (the compose golden tests pin the wire format).
func sidecarIssue(kind, fileHeading, fileName, ownWordsItem, neutralItem string, labels []string, workSlug string, payload []byte, gistURL string) (title, body string, outLabels []string) {
	body = field(hWork, workSlug) +
		attachmentSection(fileHeading, fileName, payload, gistURL) +
		checkbox(hOwnWords, ownWordsItem) +
		checkbox(hNeutralVoice, neutralItem) +
		checkbox(hLicense, itemCCBySALicense)
	return "[" + kind + "] " + workSlug, body, cloneLabels(labels)
}

// CharactersIssue composes the title, body, and labels for an add-characters
// intake issue. The payload is inlined in a fenced json block unless gistURL is
// non-empty, in which case the file section links the gist raw URL.
func CharactersIssue(workSlug string, payload []byte, gistURL string) (title, body string, labels []string) {
	return sidecarIssue("characters", hCharactersFile, "characters.json",
		itemOwnWordsCharacters, itemNeutralCharacters, charactersLabels, workSlug, payload, gistURL)
}

// RecapsIssue composes the title, body, and labels for an add-recaps intake
// issue (the recaps analogue of CharactersIssue).
func RecapsIssue(workSlug string, payload []byte, gistURL string) (title, body string, labels []string) {
	return sidecarIssue("recaps", hRecapsFile, "recaps.json",
		itemOwnWordsRecaps, itemNeutralRecaps, recapsLabels, workSlug, payload, gistURL)
}

// WorkIssue composes the title, body, and labels for an add-work intake issue.
// Required headings (Title, Author(s), Language, Narrator(s), Abridged?,
// Sources) and both confirmation checkboxes always render; optional headings
// render only when their field is non-empty.
func WorkIssue(p CoreProposal) (title, body string, labels []string) {
	var b strings.Builder
	b.WriteString(field(hTitle, p.Title))
	b.WriteString(optField(hSubtitle, p.Subtitle))
	b.WriteString(field(hAuthors, joinList(p.Authors)))
	b.WriteString(field(hLanguage, p.Language))
	b.WriteString(optField(hFirstPublished, p.FirstPublished))
	b.WriteString(optField(hSeriesName, p.SeriesName))
	b.WriteString(optField(hSeriesPosition, p.SeriesPosition))
	b.WriteString(optField(hISBN, joinList(p.PrintISBNs)))
	b.WriteString(field(hNarrators, joinList(p.Narrators)))
	b.WriteString(field(hAbridged, abridgedValue(p.Abridged)))
	b.WriteString(optField(hRuntime, runtimeValue(p.RuntimeMin)))
	b.WriteString(optField(hReleaseDate, p.ReleaseDate))
	b.WriteString(optField(hPublisher, p.Publisher))
	b.WriteString(optField(hASINs, asinLines(p.ASINs)))
	b.WriteString(optField(hAudiobookISBNs, joinList(p.AudiobookISBNs)))
	b.WriteString(optField(hCoverURL, p.CoverURL))
	b.WriteString(field(hSources, p.Sources))
	b.WriteString(checkbox(hFactualData, itemFactualData))
	b.WriteString(checkbox(hPublicDedication, itemCC0Dedication))
	return "[work] " + p.Title, b.String(), cloneLabels(workLabels)
}

// --- rendering helpers (mirror internal/issueform's test `field` helper:
// "### <label>\n\n<value>\n\n", with an empty value rendered as GitHub's
// "_No response_" sentinel) ---

// field renders one form field section. An empty value renders as the
// "_No response_" sentinel metaissue's parser maps back to "".
func field(label, value string) string {
	if value == "" {
		value = "_No response_"
	}
	return "### " + label + "\n\n" + value + "\n\n"
}

// optField renders a field only when its value is non-empty (the optional
// headings), omitting the section entirely otherwise.
func optField(label, value string) string {
	if value == "" {
		return ""
	}
	return field(label, value)
}

// checkbox renders a checkboxes field as a single ticked item.
func checkbox(label, item string) string {
	return "### " + label + "\n\n- [x] " + item + "\n\n"
}

// attachmentSection renders a sidecar file field: a gist link when gistURL is
// set, else the payload inline in a fenced json block.
func attachmentSection(label, fileName string, payload []byte, gistURL string) string {
	if gistURL != "" {
		return "### " + label + "\n\n[" + fileName + "](" + gistURL + ")\n\n"
	}
	inline := strings.TrimRight(string(payload), "\n")
	return "### " + label + "\n\n```json\n" + inline + "\n```\n\n"
}

// abridgedValue maps the tri-state Abridged to the form dropdown's value,
// defaulting an empty value to the "Unknown" option (which metaissue omits).
func abridgedValue(v string) string {
	if v == "" {
		return "Unknown"
	}
	return v
}

// runtimeValue renders a positive runtime, or "" (so the section is omitted).
func runtimeValue(min int) string {
	if min <= 0 {
		return ""
	}
	return strconv.Itoa(min)
}

// asinLines renders region-scoped ASINs as "region: ASIN" lines for the textarea.
func asinLines(asins []RegionASIN) string {
	var lines []string
	for _, a := range asins {
		region := strings.TrimSpace(a.Region)
		asin := strings.TrimSpace(a.ASIN)
		if region == "" || asin == "" {
			continue
		}
		lines = append(lines, region+": "+asin)
	}
	return strings.Join(lines, "\n")
}

// joinList joins a name/identifier list as a comma-separated field value,
// dropping empties.
func joinList(items []string) string {
	return strings.Join(nonEmpty(items), ", ")
}

// nonEmpty returns the trimmed, non-empty entries of items.
func nonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s := strings.TrimSpace(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cloneLabels returns a copy so a caller cannot mutate the package's label
// slices.
func cloneLabels(labels []string) []string {
	out := make([]string, len(labels))
	copy(out, labels)
	return out
}
