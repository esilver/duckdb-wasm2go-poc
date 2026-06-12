# DuckDB sqllogictest Runner

This directory is a standalone Go module for rerunning DuckDB's
`test/sql/**` sqllogictest corpus against the published pure-Go DuckDB driver.
It intentionally imports `github.com/esilver/duckdb-go-pure` instead of the
local generated `converge/genpkg` or `converge/genopt` packages, so the runner
works from a fresh checkout of this repository.

Fetch the DuckDB v1.5.3 corpus:

```sh
git clone --depth=1 --branch v1.5.3 --filter=blob:none --sparse \
  https://github.com/duckdb/duckdb.git duckdb-src
(cd duckdb-src && git sparse-checkout set test/sql data)
```

Build the runner:

```sh
cd converge/cmd/sqllogic
GOWORK=off GOTOOLCHAIN=go1.26.2 go build -o /tmp/sqllogic-pure .
```

Run from `duckdb-src` so corpus-relative `data/...` paths resolve:

```sh
cd ../../../duckdb-src
/tmp/sqllogic-pure -dir test/sql -timeout 30s -j 4 -v \
  > /tmp/sqllogic-rerun.txt
```

To test an unpublished local driver build, temporarily add a local `replace`
for `github.com/esilver/duckdb-go-pure` in this directory's `go.mod`, then
remove it before committing.
