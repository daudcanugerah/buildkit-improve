# Stage 4: dirfd + openat2 apply (kernel-enforced path resolution)

Landed after stage 3 findings (§0a/§0c in stage-next.md): path resolution
was ~35% of apply wall; prototype with O_PATH dirfd cache + openat2 hit
2.6×. This stage integrates the idea into the real `applyNaive` with full
OCI-diff semantics.

## Files

- `pkg/archive/dirfd_cache_unix.go` (new): O_PATH dirfd cache per
  directory. One-shot whole-path resolution via
  `openat2(RESOLVE_IN_ROOT)` (kernel-enforced chroot semantics,
  equivalent to `fs.RootPath`: in-root symlinks followed, escapes
  clamped). mkdir-on-ENOENT component walk for new trees.
- `pkg/archive/tar.go` (modified):
  - dirfd fast path for TypeReg ≤1 MiB: `openat2(RESOLVE_BENEATH|NO_SYMLINKS)`
    create, payload streamed straight from tar reader (no pooled buffer,
    no worker pool), metadata via `fchownat/fchmod/utimensat AT_EMPTY_PATH`
    with `/proc/self/fd/<df>` canonical-path fallbacks.
  - Worker pool made lazy: with the fast path the pool rarely gets work;
    eager start + per-drain teardown showed as futex/schedule storm
    (profiled ~50% CPU on dir-heavy tars).
  - `fs.RootPath` skipped on the fast path (kernel enforces containment);
    lexical paths joined under root, absolute archive names (/etc/x)
    normalized via `hasPathPrefix` check.
  - Whiteout removals, hardlinks, and metadata fallbacks resolve the
    canonical on-disk parent through the dirfd (`/proc/self/fd/<df>`),
    because raw symlink targets in archives may be absolute (host-root)
    and the stock code relied on RootPath-resolved paths everywhere.
  - ENOSYS (kernel <5.6) falls back to the stock serial/pool path.

## Measured (12-core WSL2, cgroup 2-CPU quota, tmpfs, taskset-pinned, best-of-5, sequential A/B)

| test | stock v2.4.2 | patched | speedup |
|---|---|---|---|
| 20k files, grouped | 1.067 s | 0.725 s | 1.47× |
| 20k files, interleaved | 1.272 s | 0.852 s | 1.49× |
| 100k files, grouped | 5.484 s | 4.056 s | 1.35× |
| 100k files, interleaved | 7.025 s | 4.447 s | 1.58× |

Below the 2.6× prototype: production keeps whiteout/hardlink/xattr
semantics and the RootPath-equivalent guarantees; the prototype skipped
all of those. Consistent ≥1.35× with full correctness.

Bench: `/home/daudcanugerah/office/bench/apply-bench{,-stock}`
(persistent — /tmp tmpfs wiped twice during development).

## Bugs found during development (all caught by the archive test suite)

1. `/tmp`-wipe casualties aside, the real semantic bugs:
2. Absolute archive names (`/etc/passwd`) leaked out of root — lexical
   join must normalize them under root (`hasPathPrefix`, not `IsAbs`).
3. `RESOLVE_IN_ROOT|RESOLVE_BENEATH` combined = EINVAL. Use IN_ROOT alone
   for dirfd resolution; BENEATH only for final-component creates.
4. Memoized RootPath results are absolute; re-joining root onto them
   double-prefixed. Join only when lexical (prefix check).
5. Symlink parents: path-based `mkparent` Lstat-walks see a symlink as
   ENOTDIR. Parent ensure must go through the dirfd (IN_ROOT follows
   in-root symlinks like RootPath did). Dead-symlink parents (whiteouts
   under `/dne/...`) fall back to no-op behavior for whiteout entries.
6. `link(2)` doesn't follow a final symlink but intermediate raw symlinks
   (absolute targets) point at the HOST root — hardlink destination must
   be canonicalized via the dirfd. Same for whiteout `RemoveAll`.
7. Metadata fallback paths (`lchown` etc.) must use the canonical
   location, not the lexical path that may traverse a host-pointing
   symlink.

## Test status

Full containerd archive suite (`tar_test.go`, `tar_unix_test.go` from
v2.4.2), `-race -count=3`: all pass, including all `TestBreakouts`
(symlink/hardlink escape attempts), `TestRepeatedOpaqueWhiteout`,
`TestWhiteoutSymlinkPath`, `TestDiffApply` fstest suite.

## Notes for upstreaming

The diff is self-contained: one new file + ~120 lines of tar.go changes.
 vendored patch dies on next containerd bump — upstream PR to
containerd `pkg/archive` is the durable path; stage-{1..4} docs are the
benchmark evidence.
