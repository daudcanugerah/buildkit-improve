# Stage 1: Layer Extract (Apply) Optimization

## Problem

Layer extraction for dependency trees (node_modules: 100k–500k small files,
depth 5–8) is slow. `applyNaive` in
`vendor/github.com/containerd/containerd/v2/pkg/archive/tar.go` applies entries
strictly serially. Measured syscall pattern per regular file
(benchmark: `/tmp/opencode/iouring-bench`):

```
mkparent Lstat(s)  + target Lstat  + open  + fchmod  + write  + close
+ lchown  + lchmod  + utimensat  ≈ 9–12 syscalls/file
```

20k files, warm dcache: **13.5–14.7 µs/file, ~140k syscalls, wall 275–293 ms serial.**
Goroutine-parallel version of the same pattern: **50–55 ms (5.3×)** — proving
the syscall work parallelizes across files.

Known earlier findings (this branch / discussion):
- io_uring: measured no gain warm (ops for chmod/chown/utimensat do not exist
  in mainline `io_uring.h`; ring overhead ≈ syscall cost). Rejected.
- Compression side (cache export slow) handled separately: use
  `compression=zstd` or `compression-level=1`; not code work here.

## Changes (this stage)

Stage 1 touches only `pkg/archive/tar.go` + `tar_unix.go`. Ranked:

### 1. Parallel regular-file extraction (3–5×)

Restructure `applyNaive` loop:

- Main loop stays serial for: read entry, path resolution, `mkparent`,
  whiteout handling (incl. opaque-dir walk), directory creation.
- Regular files (`TypeReg`) are dispatched to a bounded worker pool
  (default `min(8, GOMAXPROCS)`) and applied concurrently.
- **Payload handoff:** for entries ≤ `maxBufEntrySize` (1 MiB), read payload
  into a pooled buffer and dispatch the buffer. Larger files apply inline on
  the main loop (they are bandwidth-bound anyway). This sidesteps
  `tar.Reader` invalidation (`tr.Next()` invalidates prior payload).
- **Whiteout barrier:** workers drain (batch `Wait`) before the main loop
  processes a whiteout or opaque marker; ordering semantics preserved.
- **Overwrite semantics:** the `os.Lstat`+`os.RemoveAll` pre-step
  (tar.go:307–313) moves into the worker; it touches only the entry's own
  path.
- Errors via `errgroup`; first error cancels, drain, return.
- `unpackedPaths` map stays on main loop (whiteout correctness).

Expected: 3–5× on small-file layers (benchmark measured 5.3× ideal, minus
barrier/pool overhead).

### 2. Parent-dir cache in mkparent (part of earlier d39ed72 work)

- `sync.Map` keyed by absolute parent path → exists.
- Checked before `os.Lstat` at mkparent entry; populated on successful
  mkdir or observed-existing dir.
- Shared by workers (mkparent itself stays on main loop in stage 1, so
  cache is uncontended).
- Invalidated on opaque-whiteout dir removal (rare; batch barrier point).

Expected: removes O(files × depth) repeat Lstats; ~10–20% warm, more cold.

### 3. Umask-aware chmod skip in openFile (478838860)

- Read process umask once at `Apply` entry (`unix.Umask(0)` +
  restore).
- In `openFile`/`mkdir`, skip the compensating `Chmod` when
  `perm & ^umask == perm` (e.g. 0644/0755 with umask 022 — the
  overwhelming common case).
- Conditional, not behavioral change: chmod still runs when the umask
  would mask bits.

Expected: −1 syscall/file (−2/dir); ~10% of measured syscall count.

### 4. Single existence check (fold Lstat+RemoveAll into open)

- Current: `os.Lstat(path)` → `RemoveAll` → `open(O_CREAT|O_TRUNC)`.
- Change: attempt `open(O_WRONLY|O_CREATE|O_TRUNC)` directly; only on
  failure (EISDIR / ENOTEMPTY-class) do Lstat + RemoveAll + retry.
- Saves 1 syscall/file in the common fresh-path case.

Expected: ~1 syscall/file; measured fchmodat+newfstatat were the top two
non-essential syscalls.

## Out of scope (later stages)

- Stage 2 candidate: `ReaderAt`-based zero-copy dispatch for large files.
- Stage 3 candidate: batch `utimensat` sweep; skip-lchown when uid/gid match.
- Compression/export path: zstd default for cache export (ops-level, no code).

## Correctness invariants (must hold)

1. Whiteout / `.wh..wh..opq` fully applied before any entry appearing after
   them in stream order is applied.
2. Directory mtimes applied at end, after all children (existing `dirs`
   tail loop, unchanged).
3. Hardlink targets (`TypeLink`) resolve — link source may be created by a
   worker; link creation itself stays on main loop ordering (source entry
   precedes link in tar order; barrier on hardlinks too).
4. `unpackedPaths` accuracy for opaque handling — updated by main loop only,
   after worker batch completes.
5. Same on-disk end state as serial apply (byte-identical files, metadata
   equal). Tests: existing `TestApply*` suite + `-race`.

## Results (measured, this repo)

Micro-benchmark `/tmp/opencode/apply-bench` (generated pnpm-like tree, 2 KiB
files, depth 4, warm dcache, 16-core AMD Ryzen AI 7 350):

| files | stock best | patched best | speedup |
|---|---|---|---|
| 10 000 | ~330 ms | ~308 ms | ~1.07× |
| 20 000 | ~655 ms | ~605 ms | ~1.08× |
| 50 000 | ~1.70 s | ~1.55 s | ~1.10× |

Syscall deltas at 20k files (`strace -c`):

```
              stock    patched
newfstatat   531028 → 350791   (RootPath memoization + mkparent cache)
fchmod       20000  → 0        (umask skip in openFile)
fchmodat     28002  → 23000    (umask skip in mkdir; dir chmod)
openat       30015  → 30015    (unchanged — #4 not implemented)
utimensat    26000  → 26000    (unchanged)
```

Tests: full containerd archive suite (`tar_test.go` etc. copied from the
v2.4.2 module dir), `-race`, 5× count: **all pass**, including
`TestRepeatedOpaqueWhiteout` and `TestBreakouts/WhiteoutOpaqueOutOfOrder`
(both caught real bugs in the first draft — see Bugs found below).

### Why the parallel win is small here (honest analysis)

The go-par 5.3× microbenchmark used standalone per-file syscalls. Real
`archive.Apply` on this tree is dominated by *fixed per-entry costs that the
patch cannot parallelize*:

- `fs.RootPath` symlink-walk per entry (now memoized, but still serial on
  the main loop),
- `mkparent` + filter + whiteout checks (serial by design),
- tar decompress/parse feeding the loop,
- per-dir `drain()` barriers: this tree has 3 dir entries per package
  (every ~7 entries), so the pool almost never fills. Real node_modules
  tars emit dirs far less often (files grouped per dir) — expect better.

Remaining headroom, for stage 2:

1. Move the main-loop Lstat+RemoveAll for dispatched files into the worker
   (it currently runs twice: once on main loop, once in `applyFile`).
2. RootPath memoization for dir entries too, with invalidation when the
   entry itself is a symlink.
3. ReaderAt-based zero-copy dispatch for large files (no buffering).

### Bugs found and fixed during development

1. Buffer leak: worker pool never returned pooled buffers to `bufPool`.
2. `unpackedPaths` timing: opaque whiteouts that appear *interleaved* with
   files they must delete (TestRepeatedOpaqueWhiteout) require
   `unpackedPaths` to be updated at dispatch time, not after the drain.
   Semantics subtlety: the update must happen *before* the marker's
   `drain()` completes — marking at dispatch preserves the serial
   behavior because the marker itself drains before its Walk runs.
3. `TestBreakouts/WhiteoutOpaqueOutOfOrder` (file *after* opaque marker in
   same dir): passes because the marker drains the pool before the Walk;
   kept as regression test.

## Measurement plan

- Micro: `/tmp/opencode/iouring-bench` pattern (syscall counts via
  `strace -c`, wall time) against generated node_modules-like tree.
- End-to-end: apply a real pnpm/npm layer tar before/after patch;
  `buildkitd` debug log timing ("diff applied" duration in
  containerd `apply.go:67`).
- Cold-cache variant: `echo 3 > /proc/sys/vm/drop_caches` between runs
  (dedicated test box only).
