# Stage 3 (next): candidate improvements

Stage 2 landed: parallel apply + memoization stack. Measured ~1.1× on the
interleaved worst-case bench, larger on real-shape tars (TBD, see below).
This file lists what to try next, ranked, with the evidence so far.

## 0d. Stage 4 shipped + image artifacts (done 2026-11-16)

Stage 4 (dirfd + openat2 production patch) landed as commit `4d3ae06`
— full technical write-up in `stage-4.md`. Summary:

**Mechanism (what changed and why):**

1. **`dirfd_cache_unix.go` (new)** — `O_PATH` dirfd cache keyed by
   archive-relative directory path. Whole-path resolution in ONE
   `openat2(RESOLVE_IN_ROOT)` syscall replaces the userspace
   `fs.RootPath` Lstat-walk (depth × stat per entry). `RESOLVE_IN_ROOT`
   = kernel-enforced chroot semantics: in-root symlinks followed,
   escapes clamped — same guarantee as RootPath, O(1) syscalls.
   ENOENT → component-wise `mkdirat` walk (0755) + re-resolve.
2. **Regular files (≤1 MiB)** — `openat2(RESOLVE_BENEATH|NO_SYMLINKS)`
   with `O_CREAT|O_TRUNC|O_NOFOLLOW`, mode in `OpenHow.Mode` (kernel does
   NOT umask openat2 mode → no compensating chmod). Payload streamed
   directly from the tar reader — no pooled buffer copy. Metadata via
   `fchownat(fd,"",AT_EMPTY_PATH)`, `fchmod(fd)`,
   `utimensat(fd,"",AT_EMPTY_PATH)` — zero path resolution.
3. **Lazy worker pool** — the fast path applies inline; the stage-1/2
   pool starts only if the fallback dispatches. Profiling the first
   integration attempt showed `futex` 20% + `schedule` 17% CPU: the pool
   was torn down and respawned on EVERY dir entry (`drain()` barrier,
   2500 dirs × 8 workers). Lazy start eliminated it.
4. **Canonical-path fallbacks** — raw symlink targets in archives can be
   absolute (`tc.Symlink("/etc", "localetc")` → on-disk link target is
   HOST `/etc`). Stock code was safe because it applied everything
   through RootPath-resolved paths. The dirfd path resolves through the
   symlink IN-ROOT (kernel clamp), but any path-based metadata op
   (`lchown`, `link`, `RemoveAll` whiteouts) traverses the raw host
   target. Fix: resolve the real on-disk location once via
   `os.Readlink("/proc/self/fd/<df>")` + base name, use that for all
   path-based ops. This closed: hardlink EXDEV, whiteout no-op misses,
   metadata ENOENT, and prevents accidentally touching host files.
5. **ENOSYS fallback** — probe `openat2` once at cache init (kernel
   <5.6); on failure the whole apply uses the stock serial/pool path.

**Measured (12-core WSL2, 2-CPU cgroup quota, tmpfs, taskset-pinned,
best-of-5, sequential A/B — stock v2.4.2 vs patched):**

| test | stock | patched | speedup |
|---|---|---|---|
| 20k files, grouped | 1.067 s | 0.725 s | 1.47× |
| 20k files, interleaved | 1.272 s | 0.852 s | 1.49× |
| 100k files, grouped | 5.484 s | 4.056 s | 1.35× |
| 100k files, interleaved | 7.025 s | 4.447 s | 1.58× |

Gap vs prototype (2.6×): production retains whiteout/hardlink/xattr/
opaque-dir semantics, per-entry filter, dirs tail-pass, and canonical
path resolution — the prototype skipped all of that. Remaining serial
costs: tar parse (~6%), apply syscalls (~27% — now fd-relative), buffer
+ map + reader overhead (~30%).

**Correctness:** full containerd archive suite (`tar_test.go`,
`tar_unix_test.go` v2.4.2), `-race -count=3`: all pass, including every
`TestBreakouts` escape case (SymlinkAbsolute, SymlinkUpAndOut,
HardlinkDownAndOut, WhiteoutDeadSymlinkParent, ...),
`TestRepeatedOpaqueWhiteout`, `TestWhiteoutSymlinkPath`, `TestDiffApply`
fstest suite. 7 semantic bugs found and fixed during integration
(listed in `stage-4.md`).

**Artifacts:**

- Image `docker.io/daudcanugerah/buildkit-base:patch-1.2`
  (digest `sha256:2221230d...`) — buildkitd from `main` incl. stage 4.
  `patch-1` was stages 1–2 only.
- CI: `.github/workflows/image-publish.yml` (GHCR) + Docker Hub
  workflow; tag-triggered `v*` + manual dispatch.
- Bench (persistent): `~/office/bench/apply-bench{,-stock}` — `-n -iters
  -shape grouped|interleaved`. Stock binary replaces containerd to the
  pristine module cache copy.

**Next when resuming:**

1. Real-layer benchmark (actual pnpm/npm layer blob) — validates 1.35×+
   against production shape and sizes (median file size, dir grouping).
2. Upstream PR to containerd `pkg/archive` — vendored patch dies on next
   bump; stage-{1..4}.md + the numbers above are the PR body.
3. Optional: extend fast path to whiteout `unlinkat` + hardlink `linkat`
   (currently only file create + metadata are fd-relative; removals and
   links canonicalize via `/proc/self/fd` — one extra readlink each).

## 0c. dirfd + openat2 prototype (done 2026-11-16 — result: 2.6–2.8×, MAJOR)

Follow-up to sysprobe findings (§0a below). Hypothesis: path resolution
(per-entry Lstat walk + absolute open) dominates, not syscalls per se.
Prototype: minimal apply with `O_PATH` dirfd cache per parent dir,
`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)` relative opens,
`fchownat/futimesat AT_EMPTY_PATH`, `mkdirat/openat2` — single-threaded,
~150 lines (`/tmp/opencode/dirfd-apply`, uncommitted). Regular files +
dirs only, no whiteouts/symlinks/hardlinks/devices.

| test | stock | dirfd prototype | speedup |
|---|---|---|---|
| 20k grouped | 1.159 s | 0.440 s | 2.64× |
| 100k grouped | 6.02 s | 2.25 s | 2.67× |
| 20k interleaved | 1.357 s | 0.491 s | 2.77× |

~22 µs/file constant, matches sysprobe prediction (resolution ≈19 µs
removed). Single-threaded — parallelism not even needed. Kernel 6.18
(WSL2), tmpfs, warm dcache.

**This invalidates the stage-1/2 direction: a single-threaded dirfd apply
beats the parallel worker pool by ~2.5× on the same bench.** Next step:
production patch in vendored `pkg/archive` (whiteouts via unlinkat,
hardlinks via linkat, symlinks, ENOSYS fallback, full TestBreakouts).

Bugs during prototype: sync.Mutex self-deadlock in recursive
`get()` (non-reentrant) — fixed with `getLocked` internal recursion.

## 0b. CI/CD: image publish to GHCR (added)

## 0. Bench tar-shape experiment (done 2026-11-16 — result: STOP code work)

Recreated bench (`/tmp/opencode/apply-bench`, interleaved vs grouped
shapes, 2 KiB files, best-of-3/5, 12-core Ryzen 5 7535U, /tmp tmpfs).
Sequential runs only (parallel invocations invalidate results).

| shape | n | stock | patched | delta |
|---|---|---|---|---|
| interleaved | 20k | 1.400 s | 1.733 s | patched −19% |
| grouped | 20k | 1.054 s | 1.104 s | patched −5% |
| grouped | 50k | 2.72 s | 2.92 s | patched −7% |
| grouped | 100k | 6.25 s | 6.97 s | patched −11% |
| interleaved | 100k | (cut) | (cut) | — |

One 100k paired run (invoked in parallel, CPU-contended — indicative only):
stock 8.04 s vs patched 7.64 s (+5%) — did not reproduce when run
sequentially. Conclusion: **the earlier ~1.1× result does not reproduce on
this machine.** Grouped shape does NOT unlock the parallel win (pool fills
but per-file overhead of pool + buffer copy outweighs it here). Per the
stage-next decision rule: remaining time is decompress+parse and fixed
serial costs → **shift effort to ops-level levers (zstd, cache mounts,
pnpm store layer) and stop code work** on the apply path.

Caveats: synthetic bench, warm dcache, tmpfs. A real-layer run (item 1)
could still revise this — but the burden of proof is now on showing the
patch wins anywhere before keeping it.

Verdict: openat2 + dirfd cache worth investigating → see §0c.

## 0a. Cost-layer probe (done 2026-11-16 — evidence for §0c)

No strace available (no sudo). Isolated cost layers directly
(`/tmp/opencode/sysprobe`, 20k files, 2 KiB, warm tmpfs):

| layer | µs/file | share of ~53 µs apply wall |
|---|---|---|
| tar parse (headers+discard) | 3.1 | ~6% |
| raw apply syscalls (open/write/fchmod/fchownat/futimesat/close) | 14.5 | ~27% |
| path resolution (4×Lstat + full-path open) | 18.7 | ~35% |
| remainder (buffers, maps, tar.Reader, GC) | ~17 | ~31% |

Caveats: pre-existing warm tree, no whiteouts/RootPath. Order of
magnitude held through prototype (§0c).

## 0b. CI/CD: image publish to GHCR (added)

`.github/workflows/image-publish.yml`:
- Trigger: every `v*` tag push + manual dispatch.
- Builds Dockerfile target `buildkitd` (multi-arch: amd64+arm64 via QEMU).
- Pushes to `ghcr.io/<owner>/<repo>` with tags: semver `{{version}}`,
  `{{major}}.{{minor}}`, `latest` (tag builds only).
- Push only on tag events; dispatch = local load (dry-run).
- Uses `GITHUB_TOKEN` (packages: write) — no extra secrets needed.
- GHA cache scoped to `image`.

First publish makes the package private-by-default; flip
`ghcr.io/<owner>/<repo>` → Public in repo → Packages settings if wanted.


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
