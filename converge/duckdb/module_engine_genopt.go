//go:build genopt

package duckdb

// Optimized engine: scripts/transform_genopt.py shards genpkg/gen.go's 42.5k
// fnNNN methods into duckdbconverge/genopt/{core,shardNN} so each package is
// small enough for the Go optimizer (compiler memory is per-package). Build
// recipe — per-package flags, serial (rebuild_fs_all.sh step 4c, GENOPT=1):
//
//	go build -tags genopt -p 1 \
//	  -gcflags='duckdbconverge/genopt/...=-l -c=1' \
//	  -gcflags='duckdbconverge/genopt/core=-c=1' ./duckdb/...
//
// '-l' on the shards is MANDATORY: default inlining re-expands the IR and the
// compile OOMs at ~50GB. For the same reason genopt MUST be generated from the
// PRE-textual-inlining gen.go (rebuild_fs_all.sh runs the transform before the
// inline_helpers.py step): source-level pre-expansion defeats '-l' identically
// (>28GB observed). Result: ~131s / 2.75GB peak, 2.0-2.6x faster queries than
// the -N -l genpkg engine of the same (pre-inline) source.

import (
	engine "duckdbconverge/genopt/core"

	// The shards register every TBL_FnN func-var in core via their init()s;
	// without this blank import the engine's function table is all-nil.
	_ "duckdbconverge/genopt/all"
)

type engineModule = engine.Module

var engineNew = engine.New
