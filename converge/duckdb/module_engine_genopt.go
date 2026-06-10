//go:build genopt

package duckdb

// Optimized engine: scripts/transform_genopt.py shards genpkg/gen.go's 42.5k
// fnNNN methods into duckdbconverge/genopt/{core,shardNN} so each package is
// small enough for the Go optimizer (compiler memory is per-package), then
// scripts/split_giant_fns.py splits every >8k-line function (4k in shard20)
// so the inliner's IR stays bounded. Build recipe — per-package flags, serial
// (rebuild_fs_all.sh steps 4c+4d, GENOPT=1):
//
//	go build -tags genopt -p 1 \
//	  -gcflags='duckdbconverge/genopt/...=-c=1' ./duckdb/...
//
// NO '-l' anywhere: the function split (step 4d) is what makes default
// inlining feasible — before it, inlining the giant transpiled functions
// re-expanded the IR and the compile OOMed at ~50GB (peaks now 0.4-3.4GB per
// package, worst shard6). genopt MUST still be generated from the
// PRE-textual-inlining gen.go (rebuild_fs_all.sh runs the transform before the
// inline_helpers.py step): source-level pre-expansion explodes the optimizer
// identically (>28GB observed). Result: 2.3-2.9x faster queries than the
// -N -l genpkg engine of the same (pre-inline) source (1.9x on the sqllogic
// corpus wall clock).

import (
	engine "duckdbconverge/genopt/core"

	// The shards register every TBL_FnN func-var in core via their init()s;
	// without this blank import the engine's function table is all-nil.
	_ "duckdbconverge/genopt/all"
)

type engineModule = engine.Module

var engineNew = engine.New
