#!/usr/bin/env python3
"""Export a historical extraction work dir's embedded Python data tables into this
repo's JSON contracts (corrections.json / spellings.json), WITHOUT hand-transcription.

Usage:
    python3 scripts/export-extraction-data.py <work-dir> <out-dir>

The historical per-book scripts (apply_corrections.py / generate_spellings.py) each
embed a large per-book data table (the RULES list, the LEDGER, CHUNK_ENDS, the
clusters) alongside a generic engine. The Go port in internal/spelling split the two:
the engine is code, the per-book data is JSON. This script imports a historical work
dir's data tables and dumps them as the JSON the Go engine loads, so the golden tests
replay the real books against the Go engine with the data coming from the original
Python source at runtime - never hand-copied into the repo. It writes:

    <out-dir>/corrections.json   always (from apply_corrections.RULES / UNRESOLVED)
    <out-dir>/spellings.json     when generate_spellings.py exists (from its tables)

Both files' field names match internal/spelling/spelling.go's Corrections / Spellings
structs exactly. Replacement group references are converted from the Python "\\1"
style to the .NET/regexp2 "$1" style the JSON contract uses (only inside REPLACEMENT
strings; pattern backslashes like \\b / \\w are left untouched). reference_files are
NOT emitted - the golden test supplies attestation sources explicitly with absolute
paths. NonMerges live inside the historical main() (not module level), so they are
not importable; an empty list is emitted (the golden sheet comparison compares table
rows / unresolved / cluster counts only, never the non-merge footer).

Hyphens, never em dashes (workspace-wide rule).
"""
import importlib
import json
import re
import sys
from pathlib import Path

# The historical work dirs are READ-ONLY for this tool: importing their modules must
# not leave __pycache__/*.pyc bytecode behind in the extraction dir. Set before any
# sys.path import of a work-dir module.
sys.dont_write_bytecode = True


def convert_repl(repl: str) -> str:
    """Convert Python replacement group refs (\\1, and a defensive \\\\1) to the
    contract's $1 form. Only replacement strings pass through here; rule PATTERNS
    keep their backslash escapes (\\b, \\w, \\s) untouched."""
    return re.sub(r"\\+(\d)", r"$\1", repl)


def derive_title(work_dir: Path) -> str:
    """Derive a short sheet title from the work dir, e.g. hedge-wizard/work5 ->
    'HW05 verified spellings', living-forge/work3 -> 'LF03 verified spellings'."""
    parent = work_dir.parent.name  # e.g. "hedge-wizard"
    initials = "".join(part[0] for part in parent.split("-") if part).upper()
    digits = "".join(ch for ch in work_dir.name if ch.isdigit())  # "work5" -> "5"
    num = digits.zfill(2) if digits else ""
    return f"{initials}{num} verified spellings".strip()


def load_module(work_dir: Path, name: str):
    """Import a module by name from the work dir (its top level only defines data +
    functions; main() is __main__-guarded, so importing runs no pipeline). The path
    insertion stays for the process lifetime - a single export run."""
    sys.path.insert(0, str(work_dir))
    # Drop any cached module of the same name from a previous work dir.
    sys.modules.pop(name, None)
    return importlib.import_module(name)


def export_corrections(work_dir: Path, out_dir: Path) -> None:
    mod = load_module(work_dir, "apply_corrections")
    rules = []
    for pattern, repl, note in mod.RULES:
        rules.append({
            "pattern": pattern,
            "replacement": convert_repl(repl),
            "note": note,
        })
    data = {
        "rules": rules,
        "unresolved": list(getattr(mod, "UNRESOLVED", [])),
    }
    path = out_dir / "corrections.json"
    path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {path} ({len(rules)} rules, {len(data['unresolved'])} unresolved)")


def export_spellings(work_dir: Path, out_dir: Path) -> None:
    mod = load_module(work_dir, "generate_spellings")
    ledger = []
    for canonical, typ, status, carryover, variants, note in mod.LEDGER:
        ledger.append({
            "canonical": canonical,
            "type": typ,
            "status": status,
            "carryover": bool(carryover),
            "variants": variants,
            "note": note,
        })
    # Older work dirs (pre-cluster books) have no CLUSTERS table.
    clusters = [{"names": list(names), "text": text}
                for names, text in getattr(mod, "CLUSTERS", [])]
    data = {
        "title": derive_title(work_dir),
        "chunk_ends": list(mod.CHUNK_ENDS),
        "preamble": [],
        "ledger": ledger,
        "unresolved": list(getattr(mod, "UNRESOLVED", [])),
        "clusters": clusters,
        # NonMerges live inside the historical main(), not at module level, so they
        # are not importable; an empty list is faithful for the golden comparison,
        # which never inspects the non-merge footer.
        "non_merges": [],
    }
    path = out_dir / "spellings.json"
    path.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {path} ({len(ledger)} ledger, {len(clusters)} clusters, "
          f"{len(data['unresolved'])} unresolved)")


def main() -> None:
    if len(sys.argv) != 3:
        sys.exit("usage: export-extraction-data.py <work-dir> <out-dir>")
    work_dir = Path(sys.argv[1]).resolve()
    out_dir = Path(sys.argv[2]).resolve()
    if not work_dir.is_dir():
        sys.exit(f"work dir not found: {work_dir}")
    out_dir.mkdir(parents=True, exist_ok=True)

    if not (work_dir / "apply_corrections.py").is_file():
        sys.exit(f"no apply_corrections.py in {work_dir}")
    export_corrections(work_dir, out_dir)

    if (work_dir / "generate_spellings.py").is_file():
        export_spellings(work_dir, out_dir)
    else:
        print("no generate_spellings.py; skipping spellings.json")


if __name__ == "__main__":
    main()
