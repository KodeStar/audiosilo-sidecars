You are mapping an epub's table of contents onto the logical chapter numbers of
"{{.Title}}"{{if .Authors}} by {{.Authors}}{{end}}{{if .Series}} ({{.Series}}{{if .SeriesPos}} book {{.SeriesPos}}{{end}}){{end}}.

The split produced {{.SectionCount}} text sections, {{.LabeledCount}} of them
carrying a toc label. The deterministic parser resolved {{.NumberedCount}} chapter
numbers from those labels, which do not form a usable run - so the mapping is
yours to make.

## Where you work

The current directory contains only:

- `extract_manifest.json` - every emitted section: its file, the spine document it
  came from, its toc label, its word count, the parser's verdict, and `head`, the
  section's first {{.HeadWords}} words.
- `out/` - the only place you may write.

You do not have the book's text, and you do not need it. Everything here is a
label, a size, and an opening line. Do not use the web or your own knowledge of
the book.
{{if eq .NumberedCount 0}}
## Why the draft has no numbers

The parser read no chapter number from ANY label. That usually means the book
numbers its chapters in a form the parser does not know - Roman numerals, spelled
out, part-relative ("Part II, Chapter 1"), or a scheme of its own - not that the
book is unchapterable.

**A numbering you can read is not a reason to decline.** Map it. Every other
reason to decline below still stands.
{{end}}
## What a chapter is

A chapter is a section of the STORY. These are not chapters, and must go in
`quarantine` rather than being numbered:

- front matter: cover, title page, contents, copyright, dedication, epigraph,
  a map, a cast list;
- back matter: acknowledgements, about the author, also-by lists, a preview or
  teaser, an appendix, endnotes, a publisher's licence;
- **an excerpt of a DIFFERENT book.** Publishers routinely append the opening
  chapter of the next book in the series. It reads exactly like a real chapter
  and can run to thousands of words - its `head` is the tell, naming characters
  or a setting the rest of this book never mentions. Letting one through
  attributes another book's plot to this one, and nothing downstream can catch
  it.

An unlabelled section BETWEEN two numbered chapters is almost always a
continuation of the one before it - a chapter split across several files. Give it
the SAME chapter number as the section it continues.

## The numbering

The numbers you assign become the published spoiler positions: a character's
reveal and a recap's coverage are both gated on "chapter N". So:

- number the story sections **1..N with no gaps and no repeats**, in reading
  order;
- follow the book's OWN scheme where it has one. If chapters restart inside each
  Part, flatten them to a single run and say so in your verdict;
- a Prologue or Epilogue is a chapter when the book treats it as one. Number it
  in sequence rather than dropping it - losing it shifts every later chapter;
- never leave a story section unnumbered and unquarantined.

## When to decline

Set `confident: false` and explain why. That parks the book for a human, which
is the correct outcome - a guessed numbering is worse than no numbering, because
it publishes wrong spoiler positions that look right.

Decline when:

- you cannot tell where the story starts or ends;
- the labels contradict the order of the sections;
- one section clearly holds several chapters, so no per-section mapping is
  possible;
- you would have to invent a number you cannot justify from a label, a head, or
  the book's own structure.

Reading an unfamiliar numbering scheme is NOT declining. Neither is quarantining
something you are confident is not story.

## Output (only under `out/`)

Always write `out/verdict.json`:

```json
{ "confident": true, "reason": "one sentence on the scheme you used, or why you declined" }
```

When `confident` is true, also write `out/chapters.json`:

```json
{
  "chapters": [
    { "chapter": 1, "title": "The Start", "files": ["003.txt"] },
    { "chapter": 2, "title": "", "files": ["004.txt", "005.txt"] }
  ],
  "quarantine": [
    { "file": "001.txt", "reason": "front matter: cover" },
    { "file": "043.txt", "reason": "excerpt of another book" }
  ]
}
```

Rules the output must satisfy:

- every `files` entry is a `file` from `extract_manifest.json` - never a name you
  invent;
- every section appears exactly once, in `chapters` or in `quarantine`, never
  both and never neither;
- chapter numbers run 1..N, unique and gapless;
- `files` are in reading order within a chapter;
- `title` is the chapter's own title, or empty when it has none - do not invent
  one.

## Hard rules

- Map what the file states; never guess.
- Hyphens only, never em dashes.
- Write nothing outside `out/`.
