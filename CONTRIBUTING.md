# Contributing

## Document names

Name Markdown files under `docs/` after their subject, using lowercase kebab-case.
For example, use `pack-format-freeze.md` or `storage-integration-plan.md`.
Do not put week, sprint or task identifiers in filenames, such as `w1`, `w02`,
`w02-hai-02`, `sprint-2` or `task-123`. Keep those identifiers in the document
body when needed for traceability. When renaming a document, update all links
and references to its old path.

`make check` runs `python3 -B scripts/check_doc_names.py` against tracked
Markdown documents. CI rejects nonconforming names. Local untracked notes are
not included; once staged, they must follow the same rule.
