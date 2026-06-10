# Internal drafts — NOT FILED upstream (user decision 2026-06-10: keep internal)

- `wasm2go-test-report.md` — show-and-tell for ncruces/wasm2go. Numbers are from
  2026-06-09 (unoptimized engine era); see the addendum at the bottom for the
  2026-06-10 state. File only on explicit go-ahead.
- `upstream-ask-draft.md` — wasm2go function-splitting contribution offer
  (working prototype shipped in this repo as scripts/split_giant_fns.py,
  pipeline step 4d). Pairs with the report; file together if ever filed.
- `goccy-googlesqlite-pr.md` — three-PR plan for upstream goccy/googlesqlite.
  If ever filed: PR 2 (standalone fixes) first.
- `duckdb-finding.md` — **FALSIFIED, do not file**: the 2026-06-10 noopt bisect
  + native CLI comparison proved both "divergences" were OUR runner bugs
  (FLOAT-expectation parsing; bare loop-var conditions), both since fixed.
  Kept as a record of the investigation.
