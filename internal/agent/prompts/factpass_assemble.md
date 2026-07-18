You are assembling the compact final knowledge sheet for "{{.Title}}"
({{.ChapterCount}} logical chapters). Independent agents have already extracted
chapter-attributed facts. Your job is to normalize and compress those notes once,
not to retell the novel.

## Where you work

- `facts/facts-ch*.md` - all current-book facts, ordered by chapter in their
  headings. These are the only source for current-book plot claims.
- `{{.SpellingSheet}}` - the final spoiler-safe spelling sheet. Use verified names
  exactly. Preserve sanctioned probable names whose notes cite an official source,
  wiki page title, multiple agreeing references, or whose spelling is an ordinary
  English name. Use role labels only for entries explicitly marked unresolved.
{{if .HasInherited}}- `knowledge-inherited.md` - the previous book's final state.
  Carry forward only returning cast and still-relevant series threads. Do not
  attribute inherited facts to a current-book chapter.{{end}}
- `out/` - the only place you may write.

There are no transcripts. Do not use the web or model memory.

## Output

Write only `out/knowledge-final.md`, with these exact top-level sections:

- `ROSTER`: lookup-worthy and plot-consequential characters. For each, record
  canonical name and aliases, earliest meaningful current-book chapter (or
  `prior book`), apparent role, a `REVEAL-SAFE` snapshot containing only facts
  known by that chapter, and a separate compact `FINAL` status. Retain the primary
  in-book name of a renamed protagonist, named characters with an unresolved debt
  or obligation, named participants with a distinct action or fate, and distinct
  co-operators who later perform different work. Do not merge people merely because
  they belong to the same group.
- `REVEALS`: major reveal timeline as `chapter -> reveal`. Include only reveals
  that affect a character card, recap, ending, or sequel handoff.
- `THREADS`: final open/closed state of the important plot threads.
- `ENDING`: a plain, compact account of the outcome and surviving major players'
  sequel-handoff state.

Deduplicate aliases and repeated facts. Record renamed people and game avatars as
one identity in the roster when the chapter facts establish continuity, but retain
both the early alias and the primary long-running in-book name plus the chapter when
that primary name becomes safe. Never reproduce chapter-by-chapter notes,
citations, dialogue, or prose scenes. Target under 2,500 words even for a long book;
omission of trivia is preferable to another cumulative transcript surrogate.

Use fresh words, a neutral reference-guide voice, and hyphens only. If the notes
conflict or leave a material uncertainty, retain `NEEDS AUDIO REVIEW` rather than
inventing a resolution.
