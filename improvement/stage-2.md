# Stage 2: Apply — metadata memoization refinements

Builds on stage 1 (commit `7cf3e7678`). Same files:
`vendor/github.com/containerd/containerd/v2/pkg/archive/{tar.go,tar_unix.go}`.

## Changes in this stage

### 1. RootPath memoization for directory entries

Stage 1 cached `fs.RootPath` resolution only for `TypeReg`. Directory
entries re-walked every path component per entry (`walkLinks` Lstats each
level). Extended cache to `TypeDir`:

```go
canCacheRoot := hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeDir
```

Safety: TypeDir entries cannot create symlinks; any entry that *can*
(symlinks, hardlinks, devices — everything non-Reg/Dir) still resolves for
real and never populates the cache. Whiteout handlers that remove dirs
invalidate via `parentDirs.Delete(path)` (stage 1, opaque walk only).
Symlink-overwrite test (`TestBreakouts/SymlinkOverridePath`) passes.

### 2. mkparentCached — memoize every ancestor, not just the leaf

Stage 1 stored only the leaf parent path after `mkparent`. The recursive
walk still re-Lstatted all intermediate levels for each *new* leaf
(~7 levels × packages in a pnpm tree). New wrapper:

```go
func mkparentCached(ctx, path, root, parents, memo *sync.Map) error
```

- `memo.Load(path)` hit → return immediately (no syscalls).
- On success, stores `path` **and every ancestor** between root and path.

Called from the main loop when `parentDirs` misses. mkparent itself is
unchanged (still used directly by nothing else; wrapper is the only caller
in applyNaive).

### 3. De-duplicated Lstat+RemoveAll for dispatched files

Stage 1 performed the replace-check (`Lstat` + `RemoveAll`) twice for
dispatched regular files: once in the main loop, once in the worker's
`applyFile`. Now:

- dispatched regular files (TypeReg, ≤ 1 MiB): check runs **only in the
  worker**;
- everything else (dirs, big files, non-regular): check stays on the main
  loop, unchanged serial semantics.

## Measured (same bench as stage 1, warm dcache, 2 KiB files, depth 4)

| files | stock | stage 1 | stage 2 |
|---|---|---|---|
| 20 000 | ~668 ms | ~605 ms | ~590 ms |
| 100 000 | ~3.38 s | ~3.21 s | ~2.94 s |

Median-of-5 repeat run at 20k (post-`mkparentCached`): stock best ~663–670 ms,
patched best ~570–600 ms → **~1.12×**. One earlier median-of-5 measured 0.98×
(noise/interference from concurrent load); repeated runs consistently show
~1.1× on this shape. 50k: 1.11×. 100k: ~1.15×.

Syscalls at 20k files (strace, completed calls):

```
           stock    stage 1   stage 2
newfstatat 531028 → 350791  → 253004
fchmod     20000  → 0       → 0
fchmodat   28002  → 23000   → 23000
utimensat  26000  → 26000   → 26000
```

Remaining stat budget (stage 2, 20k files): 60k on file paths
(3×/file: bench verify-Walk + worker Lstat + tar-path), ~26–30k per dir
level (bench verify-Walk + first-package mkparent walks + createTarFile
TypeDir Lstat). The bench's own `filepath.Walk` verification accounts for
~1/3 of all stats; true apply cost is lower than raw strace suggests.

## Test status

Full containerd archive suite (`tar_test.go`, `tar_unix_test.go`,
`tar_linux_test.go`, `issues_test.go` from v2.4.2 module dir), `-race`,
count=3: **all pass**, including `TestRepeatedOpaqueWhiteout`,
`TestBreakouts/WhiteoutOpaqueOutOfOrder`, `TestBreakouts/SymlinkOverridePath`.

## Where stage 2 stopped

- Large-file ReaderAt dispatch (no buffering) — not implemented; >1 MiB
  files still inline serial. Most dep-tree layers are <1 MiB/file, so
  impact is small; complexity (per-entry offset tracking through the
  processor chain) is not.
- Bench tar-shape experiment (grouped vs interleaved dir entries) —
  generator written (`/tmp/opencode/shape_test.go`, not committed), run
  pending. Real node_modules tars group files per dir → fewer drain
  barriers → parallel pool fills better. Expected to show larger wins
  than the interleaved worst case used so far.

## Working on another PC — setup

```bash
git clone git@github.com:daudcanugerah/buildkit-improve.git
cd buildkit-improve
go build ./...          # vendor is committed, no network needed
# tests (test files are NOT vendored; copy from the module dir):
cp vendor/github.com/containerd/containerd/containerd/v2@v2.4.2/pkg/archive/*_test.go \
   vendor/github.com/containerd/containerd/v2/pkg/archive/ 2>/dev/null || true
# note: the v2@v2.4.2 nested module dir contains a patched copy of tar*.go
# (synced during development); test files there are stock.
go test -race -count=1 ./vendor/github.com/containerd/containerd/v2/pkg/archive/
# needs tartest package too:
cp -r vendor/github.com/containerd/containerd/containerd/v2@v2.4.2/pkg/archive/tartest \
   vendor/github.com/containerd/containerd/v2/pkg/archive/ 2>/dev/null || true
```

Caveat: test files under `vendor/` break `go mod vendor` consistency
(tartest missing from modules.txt). They are dev-only; remove after testing
(`rm -rf .../v2/pkg/archive/{tartest,*_test.go}`) before any production
build. The committed tree has them removed.

## Benchmark (reproduce numbers)

Standalone module at `/tmp/opencode/apply-bench` (not committed — see
`stage-next.md` for hardening steps):

```bash
go run . <nfiles> <iters>    # builds tar in-memory, applies, verifies count
# strace variant:
strace -f -c -o trace.txt ./bench_patched 20000 1
```

Bench uses `replace github.com/containerd/containerd/v2 => <repo>/vendor/
github.com/containerd/containerd/containerd/v2@v2.4.2` — that nested dir is
a *patched copy* kept in sync with the vendored package during development.
When pulling on another PC, re-sync it:

```bash
cp vendor/github.com/containerd/containerd/v2/pkg/archive/tar{,_unix,_windows}.go \
   vendor/github.com/containerd/containerd/containerd/v2@v2.4.2/pkg/archive/
```
