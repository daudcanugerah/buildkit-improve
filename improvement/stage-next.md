# Stage 3 (next): candidate improvements

Stage 2 landed: parallel apply + memoization stack. Measured ~1.1× on the
interleaved worst-case bench, larger on real-shape tars (TBD, see below).
This file lists what to try next, ranked, with the evidence so far.

## 0. Bench tar-shape experiment (do first — cheap, decides priorities)

Current bench (`/tmp/opencode/apply-bench/main.go`) writes 3 dir entries
per package interleaved with files → `drain()` barrier every ~7 entries →
worker pool rarely fills. Real node_modules/pnpm tars group all files of a
directory before moving on → barriers ~per-directory → pool fills.

A grouped-shape generator exists (uncommitted): files contiguous per
package, dirs once per package. Wire it behind a flag
(`./bench -shape grouped`) and compare stock vs patched. Outcomes:

- **Grouped shows ≥1.5×** → parallel apply earns its complexity on real
  layers; proceed with 1–3 below.
- **Grouped still ~1.1×** → remaining time is decompress+parse and fixed
  serial costs; shift effort to 4 (ops-level) and stop code work.

## 1. Real-layer benchmark (validates everything)

Bench tars are synthetic. Apply an actual pnpm/npm layer blob:

```bash
# extract a real layer tar from the content store / docker save,
# then time archive.Apply on it before/after patch:
docker save <image> | tar -xO <layerblob>.tar > layer.tar
wc -l <(tar -tvf layer.tar | head -100000)   # entry count + shape
```

Record: entry count, dir/file ratio, median file size, dir-entry grouping.
Compare against bench assumptions. This is the only number that predicts
production behavior.

## 2. Remaining apply-path code items (small)

- **createTarFile TypeDir Lstat**: dir entries Lstat before mkdir
  (merge-check). Could try mkdir directly + EEXIST fallback — saves
  ~1 syscall/dir.
- **Tail chtimes batch**: `dirs` loop does one utimensat per dir at end —
  fine; but each dir entry ALSO paid copyDirInfo? No — dirs go through
  createTarFile (lchown+lchmod+chtimes each). Could defer all dir metadata
  (chown/chmod/utimes) to the end like mtimes: one pass, O(dirs) instead
  of interleaved with workers. Note: semantics identical because dir
  metadata only matters after all children are applied (that's why mtimes
  are deferred already) — but chown/chmod also affect file creation
  permissions inside (sticky/sgid). Risk: setgid dirs. Investigate before
  doing it.
- **Filter cost**: `options.Filter` runs per entry on the main loop;
  BuildKit's filter is trivial, no action.

## 3. Worker-pool tuning

- Pool size fixed at min(8, GOMAXPROCS). Try 4/16/32 on NVMe vs SATA.
- Buffer pool: 1 MiB max entry × 8 workers = 8 MiB resident — fine.
- Channel backpressure: `send()` blocks main loop when workers are
  saturated — that's the intended throttle; verify it doesn't deadlock on
  very deep drain chains (race tests cover this).

## 4. Ops-level levers (usually bigger than all code work)

- **zstd layers** (`compression=zstd`): decompress 3–5× faster than gzip;
  extraction is often decompress-bound, not syscall-bound. Zero code.
- **cache mounts** for node_modules: layer never re-applied.
- **pnpm store as stable layer**: unchanged layer → snapshot cache hit →
  apply skipped entirely (ApplyLayers stats chain first).
- **eStargz** only if lazy remote pulls matter.

## 5. Upstream

If grouped-shape bench shows real wins, upstream the diff to
containerd `pkg/archive` (parallel apply PR). The vendored copy will be
replaced on the next `go mod vendor` / version bump — upstreaming is the
only durable path. Keep the diff minimal and benchmark-backed:
`improvement/stage-{1,2}.md` numbers are the PR body.

## Status snapshot (for picking up on another PC)

- Stage 1+2 committed and pushed to `origin` (buildkit-improve), branch `main`.
- Working tree clean except `improvement/` docs (committed with stage 2).
- Bench module: `/tmp/opencode/apply-bench/` — recreate from stage-2.md
  setup section; needs `replace` to the nested patched containerd dir.
- Nested patched copy: `vendor/github.com/containerd/containerd/containerd/
  v2@v2.4.2/pkg/archive/` mirrors the vendored package (bench replace
  target). Re-sync with the cp command in stage-2.md.
