You are extracting compact, chapter-attributed facts from {{if .IsEbook}}the book{{else}}the audiobook{{end}}
"{{.Title}}", covering chapters {{.From}} through {{.To}}. A later synthesis
stage will combine your notes with independently extracted chunks. Preserve the
important story evidence, but do not write narrative prose or a cumulative book
summary.
{{if .ChunkNote}}
Note on this book's structure: {{.ChunkNote}}
{{end}}

## Where you work

The current directory contains only:

{{if .SpellingSheet}}- `{{.SpellingSheet}}` - the spoiler-bounded canonical spelling sheet. Use a
  `verified` canonical exactly. You may also preserve a `probable` canonical when
  it is an ordinary English name or its note explicitly cites an official source,
  wiki page title, or multiple agreeing references - those are the audio pipeline's
  sanctioned probable-name cases. Only `unresolved` entries must be replaced by a
  role. Never discard a supported name merely because ASR originally misspelled it.
{{end}}{{if .HasInherited}}- `knowledge-inherited.md` - the PREVIOUS BOOK's compact final
  state. It is safe series context. Use it only to recognize returning characters;
  current-book facts must still come from this chunk's {{if .IsEbook}}chapters{{else}}transcripts{{end}}.{{end}}
- `{{.TextDir}}/` - chapters {{.From}} through {{.To}} only.
- `out/` - the only place you may write.

No current-book chunk before or after this one is present. That independence is
deliberate: exact chapter headings and citations, not a rolling model memory, carry
the spoiler boundary. Do not use the web or your own knowledge of the book.

{{if .IsEbook}}## Source rules

The inputs are the book's own text, exactly as published. Names, places and invented
terms are spelled correctly - use them exactly as written, and never substitute a
role label for a name you can read. There is no transcription uncertainty to flag.

Because you are reading the author's actual sentences, the risk this pass has to
manage is the opposite of the audio one: not mishearing, but carrying the prose
across. Your notes are the ONLY thing the later synthesis sees, so any phrasing you
copy here can reach a published file. Record what happened, never how it was
written: no quoted or lightly reworded sentences, no distinctive images or turns of
phrase, no dialogue. If a sentence of yours would read as the author's voice rather
than a reference guide's, rewrite it.

Write every fact in fresh, concise words. Every material bullet needs a private
citation such as `[ch12]`.
{{else}}## Audio-source rules

The inputs are ASR transcripts. They can contain omissions, homophones, false
punctuation, and uncertain proper nouns. Never guess an unresolved spelling. If
uncertainty changes a material fact, write `NEEDS AUDIO REVIEW` with the chapter
and timestamp.

Write every fact in fresh, concise words. Do not quote or lightly reword dialogue.
Every material bullet needs a private citation such as `[ch12 @ 03:10-03:28]`.
{{end}}
## Output

Write only `out/facts-ch{{.From}}-{{.To}}.md`. It must contain one `## Chapter N`
section for every chapter {{.From}} through {{.To}}, and none outside that range.
Use these compact subsections:

- `EVENTS`: 3-8 consequential chronological bullets. Omit atmosphere, repeated
  combat beats, and details that will not help a character card or recap.
- `CHARACTERS`: introductions, aliases, identity continuity, relationship changes,
  abilities, status changes, and apparent roles. Explicitly connect a renamed person
  or avatar across adjacent chapters - for example, "A is the same person as B" -
  whenever the {{if .IsEbook}}text{{else}}transcript{{end}} supports it. Label a first appearance only when it occurs
  in this chunk; the later assembler determines the earliest chapter globally.
- `STATE`: at most 3 bullets covering the protagonist's situation, important
  reader beliefs, and open threads after the chapter.
- `ACT BREAK CANDIDATE`: one short line only when the chapter is a natural recap
  boundary.

Do not emit a roster, reveal timeline, thread ledger, or cumulative knowledge
sheet. Those are assembled once after all chunks finish.

## Hard rules

- Facts only, in neutral reference-guide language.
- Exact chapter attribution; confirmation belongs to the chapter where it occurs.
- Hyphens only, never em dashes.
- Resolve, flag, or omit uncertainty - never guess.
- Read no chapter beyond {{.To}}.
