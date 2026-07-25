You are the focused semantic verification pass for the sidecars of "{{.Title}}"
({{.ChapterCount}} logical chapters). A prior adversarial audit converged in round
{{.Round}} and a fixer was instructed to apply its remaining FIX findings. Verify
that those exact corrections are now genuinely present. Do not start a fresh audit
or search for unrelated stylistic improvements.
{{if .EdgeNote}}
Note on this book's structure: {{.EdgeNote}} A stop at the last logical story
chapter ({{.ChapterCount}}) is correct; do not fault it.
{{end}}

## Where you work

- `authoring.md` - the sidecar contract.
- `audit_accepted.json` - the exact prior FIX and NIT findings. FIX findings are
  mandatory; NIT findings are context and need not be corrected.
- `sidecars/characters.json` and `sidecars/recaps.json` - the post-fix files.
- `facts/` - chapter-attributed evidence for checking the affected entries.
- `validation_report.json` - the clean mechanical revalidation report.
- `out/` - the only place you may write.

Do not use the web or model memory. Do not rewrite sidecars.

## Task

For every prior FIX finding, inspect its locus in the current sidecars and the cited
fact evidence. Confirm the offending claim is removed, corrected, or moved behind
the right spoiler boundary. Also confirm the correction did not introduce a new
spoiler or unsupported claim in that same entry.

Write `out/audit.json` in the normal audit shape. Set `pass` to true only if every
prior FIX is resolved. Emit a finding for each unresolved item with the current text,
evidence, and concrete correction. Grade it BLOCKER ONLY when it falls in one of the
four blocker classes - (a) a spoiler leak, (b) a factual contradiction with the fact
notes, (c) a position outside the logical chapter range, (d) a verbatim or licensing
violation; otherwise grade an unresolved item FIX. NITs may be reported but do not
block pass. Use fresh words and hyphens only.
