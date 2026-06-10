# wasm2go upstream ask (draft)

Subject: function splitting pass for oversized output functions

wasm2go's Go output maps each wasm function to one Go function. DuckDB's
largest transpiled function (a 36.5k-line / ~66k-SSA-block visitor) exceeds
the Go compiler's hard 65536-blocks-per-function limit, which bricks every
GOOS=js/GOARCH=wasm build, and the 8-25k-line cohort (~30 functions) forces
`-l` (inlining off) on all engine packages because the inliner's IR
re-expansion OOMs (>50GB).

Ask: an output-stage transform (we have a working prototype to contribute)
that splits any function over a line/block threshold:
1. flatten the structured output to label-segments (bare blocks spliced,
   `if`/`if-else` containing labels lowered to conditional gotos),
2. hoist declared locals into a per-call struct passed by pointer —
   CRITICAL: emit explicit zero-assignments at the original declaration
   sites, because `var x T` re-zeroes on every loop re-entry,
3. pack segments into part functions exchanging integer jump codes through
   a range-dispatch driver; cross-part `goto` becomes `return <code>`.
Measured on DuckDB: js/wasm builds work (549MB module, V8 instantiates in
284ms, full query battery passes), and every package compiles WITH inlining
at <3.5GB compiler RSS (vs >50GB OOM), worst shard 1.8GB.
