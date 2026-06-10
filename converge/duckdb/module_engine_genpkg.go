//go:build !genopt

package duckdb

// Default engine: the monolithic wasm2go output in duckdbconverge/genpkg.
// gen.go is too large for the Go compiler's optimizer (>50GB IR), so genpkg
// must be compiled -N -l (see rebuild_fs_all.sh step 5) and runs ~2-2.6x
// slower than the optimized engine. Build with -tags genopt to select the
// multi-package optimized engine instead (module_engine_genopt.go; generate
// it with GENOPT=1 rebuild_fs_all.sh or scripts/transform_genopt.py).

import engine "duckdbconverge/genpkg"

// engineModule / engineNew are the only seam between the driver and a
// generated engine package; both engine flavors export the same surface.
type engineModule = engine.Module

var engineNew = engine.New
