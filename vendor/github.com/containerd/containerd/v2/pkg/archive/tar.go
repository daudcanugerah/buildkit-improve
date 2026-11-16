/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/sys/userns"
	"golang.org/x/sync/errgroup"

	"github.com/containerd/containerd/v2/pkg/archive/tarheader"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
)

var bufPool = &sync.Pool{
	New: func() any {
		buffer := make([]byte, 32*1024)
		return &buffer
	},
}

var errInvalidArchive = errors.New("invalid archive")

// Diff returns a tar stream of the computed filesystem
// difference between the provided directories.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func Diff(ctx context.Context, a, b string, opts ...WriteDiffOpt) io.ReadCloser {
	r, w := io.Pipe()

	go func() {
		err := WriteDiff(ctx, w, a, b, opts...)
		if err != nil {
			log.G(ctx).WithError(err).Debugf("write diff failed")
		}
		if err = w.CloseWithError(err); err != nil {
			log.G(ctx).WithError(err).Debugf("closing tar pipe failed")
		}
	}()

	return r
}

// WriteDiff writes a tar stream of the computed difference between the
// provided paths.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func WriteDiff(ctx context.Context, w io.Writer, a, b string, opts ...WriteDiffOpt) error {
	var options WriteDiffOptions
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return fmt.Errorf("failed to apply option: %w", err)
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && options.SourceDateEpoch == nil {
		options.SourceDateEpoch = tm
	}

	if options.writeDiffFunc == nil {
		options.writeDiffFunc = writeDiffNaive
	}

	return options.writeDiffFunc(ctx, w, a, b, options)
}

// writeDiffNaive writes a tar stream of the computed difference between the
// provided directories on disk.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func writeDiffNaive(ctx context.Context, w io.Writer, a, b string, o WriteDiffOptions) error {
	var opts []ChangeWriterOpt
	if o.SourceDateEpoch != nil {
		opts = append(opts, WithModTimeUpperBound(*o.SourceDateEpoch))
		// Since containerd v2.0, the whiteout timestamps are set to zero (1970-01-01),
		// not to the source date epoch
	}
	cw := NewChangeWriter(w, b, opts...)
	err := fs.Changes(ctx, a, b, cw.HandleChange)
	if err != nil {
		return fmt.Errorf("failed to create diff tar stream: %w", err)
	}
	return cw.Close()
}

const (
	// whiteoutPrefix prefix means file is a whiteout. If this is followed by a
	// filename this means that file has been removed from the base layer.
	// See https://github.com/opencontainers/image-spec/blob/main/layer.md#whiteouts
	whiteoutPrefix = ".wh."

	// whiteoutMetaPrefix prefix means whiteout has a special meaning and is not
	// for removing an actual file. Normally these files are excluded from exported
	// archives.
	whiteoutMetaPrefix = whiteoutPrefix + whiteoutPrefix

	// whiteoutOpaqueDir file means directory has been made opaque - meaning
	// readdir calls to this directory do not follow to lower layers.
	whiteoutOpaqueDir = whiteoutMetaPrefix + ".opq"

	paxSchilyXattr = "SCHILY.xattr."

	userXattrPrefix = "user."
)

// Apply applies a tar stream of an OCI style diff tar.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md#applying-changesets
func Apply(ctx context.Context, root string, r io.Reader, opts ...ApplyOpt) (int64, error) {
	root = filepath.Clean(root)

	var options ApplyOptions
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return 0, fmt.Errorf("failed to apply option: %w", err)
		}
	}
	if options.Filter == nil {
		options.Filter = all
	}
	if options.applyFunc == nil {
		options.applyFunc = applyNaive
	}

	return options.applyFunc(ctx, root, r, options)
}

// maxBufEntrySize is the largest regular-file payload applied via a pooled
// in-memory buffer dispatched to a worker. Entries larger than this apply
// inline on the main loop (they are bandwidth-bound, and streaming avoids
// buffering cost).
const maxBufEntrySize = 1 << 20

// applyWorkers caps the parallel regular-file workers. Metadata syscalls
// contend on inode/journal locks beyond this on common filesystems
// (measured plateau ~5× on ext4 with 16 procs).
func applyWorkers() int {
	if n := runtime.GOMAXPROCS(0); n < 8 {
		return n
	}
	return 8
}

// applyNaive applies a tar stream of an OCI style diff tar to a directory
// applying each file as either a whole file or whiteout.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md#applying-changesets
//
// Regular files within maxBufEntrySize are applied by a bounded worker pool;
// ordering-sensitive entries (directories, whiteouts, opaque markers,
// hardlinks, symlinks, large files) are applied inline on the main loop,
// with workers drained before each such entry. This preserves stream-order
// semantics: a whiteout is always fully applied before any entry that
// follows it in the stream, and hardlink sources always exist before their
// links.
func applyNaive(ctx context.Context, root string, r io.Reader, options ApplyOptions) (size int64, err error) {
	var (
		dirs []*tar.Header

		tr = tar.NewReader(r)

		// Used for handling opaque directory markers which
		// may occur out of order
		unpackedPaths = make(map[string]struct{})
		// Used for tracking directories that have already processed an
		// opaque directory marker to avoid redundant directory walks
		opaqueDirs = make(map[string]struct{})

		// parentDirs caches parent paths already ensured to exist, skipping
		// repeated mkparent Lstat walks for sibling files in the same
		// directories. Written only by the main loop.
		parentDirs sync.Map

		convertWhiteout = options.ConvertWhiteout
	)

	setApplyUmask()

	// dirfd fast path: cache O_PATH fds per directory and apply entries
	// with *at()/openat2() relative opens. Falls back transparently to the
	// path-based serial path when openat2 is unavailable (kernel < 5.6).
	var dcache *dirfdCache
	if dc, derr := newDirfdCache(root); derr == nil {
		dcache = dc
		defer dc.closeAll()
	}

	if convertWhiteout == nil {
		// handle whiteout by removing the target files
		convertWhiteout = func(hdr *tar.Header, path string) (bool, error) {
			base := filepath.Base(path)
			dir := filepath.Dir(path)
			if base == whiteoutOpaqueDir {
				if _, ok := opaqueDirs[dir]; ok {
					return false, nil
				}
				opaqueDirs[dir] = struct{}{}

				_, err := os.Lstat(dir)
				if err != nil {
					return false, err
				}
				err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						if os.IsNotExist(err) {
							err = nil // parent was deleted
						}
						return err
					}
					if path == dir {
						return nil
					}
					if _, exists := unpackedPaths[path]; !exists {
						err := os.RemoveAll(path)
						if err != nil {
							return err
						}
						if info.IsDir() {
							// removed dirs must not be treated as
							// known-existing by the mkparent cache
							parentDirs.Delete(path)
							return filepath.SkipDir
						}
					}
					return nil
				})
				return false, err
			}

			if strings.HasPrefix(base, whiteoutPrefix) {
				originalBase := base[len(whiteoutPrefix):]
				originalPath := filepath.Join(dir, originalBase)

				// The lexical path may traverse symlinks whose raw targets
				// point outside root (absolute archive symlink names); the
				// removal must hit the in-root location. Resolve the
				// canonical parent through the dirfd cache when available.
				if dcache != nil && dcache.openat2OK {
					if pfd, perr := dcache.get(relFromRoot(root, dir)); perr == nil {
						if canonical, rerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", pfd)); rerr == nil {
							originalPath = filepath.Join(canonical, originalBase)
						}
					}
				}
				return false, os.RemoveAll(originalPath)
			}

			return true, nil
		}
	}

	// Bounded worker pool applying regular files concurrently. Each entry
	// carries its payload in a pooled buffer, so workers never touch the
	// tar reader (tr.Next() invalidates the previous entry's payload).
	nworkers := applyWorkers()
	type fileEntry struct {
		hdr    *tar.Header
		path   string
		data   []byte
		buf    *[]byte // pooled buffer backing data; returned to bufPool
		noSame bool
		df     int // cached parent dir fd (dirfd fast path); -1 when unused
	}
	files := make(chan fileEntry, nworkers*2)
	var wg sync.WaitGroup
	eg, egctx := errgroup.WithContext(ctx)
	applyFile := func(e fileEntry) error {
		defer func() {
			if e.buf != nil {
				bufPool.Put(e.buf)
			}
		}()
		if err := egctx.Err(); err != nil {
			return err
		}
		if e.df >= 0 && dcache != nil {
			// dirfd fast path: apply relative to the cached parent fd.
			// e.path remains the fallback for metadata ops and errors.
			return createTarFileAt(egctx, e.df, filepath.Base(e.path), e.path, root, e.hdr, bytes.NewReader(e.data), e.noSame, dcache)
		}
		// Overwrite semantics, same as the serial path: remove any existing
		// entry that is not a directory being merged.
		if fi, err := os.Lstat(e.path); err == nil {
			if !fi.IsDir() || e.hdr.Typeflag != tar.TypeDir {
				if err := os.RemoveAll(e.path); err != nil {
					return err
				}
			}
		}
		return createTarFile(egctx, e.path, root, e.hdr, bytes.NewReader(e.data), e.noSame, dcache)
	}
	worker := func() error {
		defer wg.Done()
		for e := range files {
			if err := applyFile(e); err != nil {
				return err
			}
		}
		return nil
	}
	// poolStarted guards lazy worker start: with the dirfd fast path the
	// fallback pool usually never receives an entry, and spinning up +
	// tearing down workers per drain() showed as futex/schedule overhead
	// in profiles of dir-heavy tars.
	poolStarted := false
	ensurePool := func() {
		if poolStarted {
			return
		}
		poolStarted = true
		for i := 0; i < nworkers; i++ {
			wg.Add(1)
			eg.Go(func() error {
				return worker()
			})
		}
	}
	// send dispatches a file entry to the pool; drain waits until every
	// dispatched entry has been applied, restoring a serial point.
	sendErr := error(nil)
	send := func(e fileEntry) error {
		if sendErr != nil {
			return sendErr
		}
		ensurePool()
		select {
		case files <- e:
			return nil
		case <-egctx.Done():
			sendErr = egctx.Err()
			return sendErr
		}
	}
	drain := func() {
		if !poolStarted {
			return
		}
		close(files)
		wg.Wait()
		sendErr = nil
		files = make(chan fileEntry, nworkers*2)
		poolStarted = false
	}

	// Iterate through the files in the archive.
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		default:
		}
		if err != nil {
			break
		}

		hdr, terr := tr.Next()
		if terr == io.EOF {
			// end of tar archive
			break
		}
		if terr != nil {
			err = terr
			break
		}

		size += hdr.Size

		// Normalize name, for safety and for a simple is-root check
		hdr.Name = filepath.Clean(hdr.Name)

		accept, aerr := options.Filter(hdr)
		if aerr != nil {
			err = aerr
			break
		}
		if !accept {
			continue
		}

		if skipFile(hdr) {
			log.G(ctx).Warnf("file %q ignored: archive may not be supported on system", hdr.Name)
			continue
		}

		// Split name and resolve symlinks for root directory.
		ppath, base := filepath.Split(hdr.Name)
		var rerr error
		// With the dirfd fast path, containment is enforced by the kernel
		// at open time (RESOLVE_BENEATH/RESOLVE_IN_ROOT on cached dirfds),
		// so the userspace symlink walk is unnecessary: ppath keeps its
		// lexical form and the entry fd is resolved relative to root.
		// Without it, fs.RootPath Lstat-walks every component of ppath to
		// resolve symlinks; identical ppaths repeat across sibling files,
		// so memoize the resolution. Only regular files and directories
		// use the cache; any other entry type may introduce a symlink and
		// must resolve for real.
		useDirfd := dcache != nil && dcache.openat2OK
		canCacheRoot := hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeDir
		if useDirfd && (canCacheRoot || hdr.Typeflag == tar.TypeLink) {
			// lexical path under root; kernel enforces containment
		} else {
			var resolved bool
			if canCacheRoot {
				if v, ok := parentDirs.Load("rp:" + ppath); ok {
					ppath = v.(string)
					resolved = true
				}
			}
			if !resolved {
				ppath, rerr = fs.RootPath(root, ppath)
				if rerr != nil {
					err = fmt.Errorf("failed to get root path: %w", rerr)
					break
				}
				if canCacheRoot {
					parentDirs.Store("rp:"+ppath, ppath)
				}
			}
		}

		// Join to root before joining to parent path to ensure relative links are
		// already resolved based on the root before adding to parent.
		// With the dirfd fast path, ppath may be lexical (archive-relative)
		// or carry a leading separator for absolute archive names like
		// /etc/passwd (RootPath used to strip it). Anything not already
		// under root is treated as archive-relative and joined under root;
		// containment is enforced at open time by
		// RESOLVE_BENEATH/RESOLVE_IN_ROOT, not by this lexical join.
		if useDirfd && !hasPathPrefix(ppath, root) {
			ppath = filepath.Join(root, ppath)
		}
		path := filepath.Join(ppath, filepath.Join("/", base))
		if path == root {
			log.G(ctx).Debugf("file %q ignored: resolved to root", hdr.Name)
			continue
		}

		// If file is not directly under root, ensure parent directory
		// exists or is created. The cache skips the per-level Lstat walk
		// for siblings in directories already ensured this run.
		if ppath != root {
			parentPath := ppath
			if base == "" {
				parentPath = filepath.Dir(path)
			}
			if useDirfd {
				// dirfd fast path: ensure the parent exists by resolving
				// it through the dirfd cache — openat2 RESOLVE_IN_ROOT
				// follows in-root symlinks (matching RootPath semantics)
				// and creates missing components (mode 0755). The
				// path-based mkparent walk would Lstat a symlink parent
				// as non-dir (ENOTDIR) instead of resolving it.
				if rel := relFromRoot(root, ppath); rel != "" {
					if _, derr := dcache.get(rel); derr != nil {
						// A dangling symlink parent (e.g. whiteout under a
						// dead symlink path) cannot be resolved; stock
						// behavior is a no-op removal for whiteouts under
						// such parents, so only fail for entries that
						// would actually be written.
						isWhiteout := strings.HasPrefix(base, whiteoutPrefix)
						if !isWhiteout {
							err = fmt.Errorf("failed to ensure parent dir %q: %w", ppath, derr)
							break
						}
					}
				}
			} else {
				if _, cached := parentDirs.Load(parentPath); !cached {
					if err = mkparentCached(ctx, parentPath, root, options.Parents, &parentDirs); err != nil {
						break
					}
				}
			}
		}

		// Ordering barrier: whiteouts, opaque markers, directories,
		// symlinks and hardlinks must be applied relative to the current
		// stream position, so wait for in-flight files first.
		if hdr.Typeflag != tar.TypeReg {
			drain()
		}

		// Naive whiteout convert function which handles whiteout files by
		// removing the target files.
		if err = validateWhiteout(path); err != nil {
			break
		}
		writeFile, werr := convertWhiteout(hdr, path)
		if werr != nil {
			err = fmt.Errorf("failed to convert whiteout file %q: %w", hdr.Name, werr)
			break
		}
		if !writeFile {
			continue
		}
		// If path exits we almost always just want to remove and replace it.
		// The only exception is when it is a directory *and* the file from
		// the layer is also a directory. Then we want to merge them (i.e.
		// just apply the metadata from the layer).
		//
		// For dispatched regular files this check is left to the worker
		// (it runs in applyFile); doing it here as well would duplicate
		// the Lstat+RemoveAll on every small file.
		if hdr.Typeflag != tar.TypeReg || hdr.Size > maxBufEntrySize {
			if fi, serr := os.Lstat(path); serr == nil {
				if !fi.IsDir() || hdr.Typeflag != tar.TypeDir {
					if rerr := os.RemoveAll(path); rerr != nil {
						err = rerr
						break
					}
					// Removal invalidates cached fds under the removed
					// path: reopened lazily on next use.
					if dcache != nil {
						dcache.invalidateChildren(relFromRoot(root, path))
					}
				} else if dcache != nil {
					// dir-over-dir merge: dir was replaced? No — merge
					// keeps it, but the Lstat'd dir could have replaced
					// a prior non-dir; keep cache consistent anyway.
					dcache.invalidate(relFromRoot(root, path))
				}
			} else if serr != nil && os.IsNotExist(serr) && dcache != nil && hdr.Typeflag == tar.TypeDir {
				// no existing entry: ensure cache reflects the dir we will
				// create via the serial path below.
				dcache.invalidate(relFromRoot(root, path))
			}
		}

		// Regular files are applied via createTarFileAt when the dirfd
		// fast path is available: streamed directly from the tar reader
		// (no pooling, no buffer copy), resolution is dirfd-relative.
		// The worker pool is skipped entirely on this path — the dirfd
		// apply removes the syscall cost the pool parallelized, and the
		// measured pool overhead (channel + buffer copy) exceeded its
		// benefit here.
		if hdr.Typeflag == tar.TypeReg && hdr.Size <= maxBufEntrySize && dcache != nil && dcache.openat2OK {
			if df, derr := dcache.get(relFromRoot(root, ppath)); derr == nil {
				if cerr := createTarFileAt(ctx, df, filepath.Base(path), path, root, hdr, tr, options.NoSameOwner, dcache); cerr != nil {
					err = cerr
					break
				}
				unpackedPaths[path] = struct{}{}
				continue
			}
		}

		// Fallback (no openat2 / dirfd error): pooled worker path.
		if hdr.Typeflag == tar.TypeReg && hdr.Size <= maxBufEntrySize {
			buf := bufPool.Get().(*[]byte)
			data := (*buf)[:hdr.Size]
			if _, rerr = io.ReadFull(tr, data); rerr != nil {
				bufPool.Put(buf)
				err = rerr
				break
			}
			e := fileEntry{hdr: hdr, path: path, data: data, buf: buf, noSame: options.NoSameOwner, df: -1}
			if serr := send(e); serr != nil {
				bufPool.Put(buf)
				err = serr
				break
			}
			// unpackedPaths must reflect files already visible for opaque
			// whiteout handling: a subsequent opaque marker in this dir
			// will drain() first, so marking here is order-safe.
			unpackedPaths[path] = struct{}{}
			continue
		}

		srcData := io.Reader(tr)
		srcHdr := hdr

		if cerr := createTarFile(ctx, path, root, srcHdr, srcData, options.NoSameOwner, dcache); cerr != nil {
			err = cerr
			break
		}
		if dcache != nil && hdr.Typeflag != tar.TypeReg {
			// Serial-applied non-regular entries may have replaced a path
			// component (symlink-over-dir breakout defense); refresh the
			// cached fd for the applied path itself. Parent fds remain
			// valid: parents were resolved via RootPath before apply.
			dcache.invalidate(relFromRoot(root, path))
		}

		// Directory mtimes must be handled at the end to avoid further
		// file creation in them to modify the directory mtime
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, hdr)
		}
		unpackedPaths[path] = struct{}{}
	}

	// Final drain: apply any in-flight entries and surface the first error
	// from either the main loop or the workers. Workers must always be
	// joined before returning so none outlives the call writing to root.
	close(files)
	wg.Wait()
	if err == nil {
		if aerr := eg.Wait(); aerr != nil {
			err = aerr
		}
	} else {
		eg.Wait()
	}
	if err != nil {
		return size, err
	}

	for _, hdr := range dirs {
		path, derr := fs.RootPath(root, hdr.Name)
		if derr != nil {
			return 0, derr
		}
		if cerr := chtimes(path, boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime)); cerr != nil {
			return 0, cerr
		}
	}

	return size, nil
}

// createTarFileAt is the dirfd fast path of createTarFile for regular
// files. df is the O_PATH fd of the (validated) parent directory; base
// is the entry's base name. The file is created with openat2
// (RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) so the kernel enforces
// containment, and all metadata is applied via fd-relative calls.
// Metadata fallbacks use path for kernels/filesystems lacking the *at
// variants. Semantics match createTarFile for TypeReg entries.
func createTarFileAt(ctx context.Context, df int, base, path, extractDir string, hdr *tar.Header, reader io.Reader, noSameOwner bool, dcache *dirfdCache) error {
	mode := uint32(hdr.Mode & 0o7777)

	// Canonical path for metadata fallbacks: the dirfd is the
	// RESOLVE_IN_ROOT-resolved parent, so /proc/self/fd/<df>/base is the
	// real on-disk location even when `path` traverses symlinks whose
	// raw targets point outside root (absolute archive symlink names).
	if resolved, rerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", df)); rerr == nil {
		path = filepath.Join(resolved, base)
	}

	fd, _, err := openAtFile(df, base, mode, dcache.openat2OK)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	_, err = copyBuffered(ctx, f, reader)
	if err1 := f.Close(); err == nil {
		err = err1
	}
	if err != nil {
		return err
	}

	if !noSameOwner {
		if err := lchownAt(fd, path, hdr.Uid, hdr.Gid); err != nil {
			return fmt.Errorf("failed to Lchown %q for UID %d, GID %d: %w", path, hdr.Uid, hdr.Gid, err)
		}
	}

	for key, value := range hdr.PAXRecords {
		if strings.HasPrefix(key, paxSchilyXattr) {
			key = key[len(paxSchilyXattr):]
			if err := setxattrAt(fd, key, value, path); err != nil {
				if errors.Is(err, syscall.EPERM) && strings.HasPrefix(key, userXattrPrefix) {
					// Only regular files and directories can carry user.*
					// xattrs; other types fall back to the serial warn+skip.
					log.G(ctx).Warnf("ignored xattr %s in archive", key)
					continue
				}
				if errors.Is(err, syscall.ENOTSUP) {
					log.G(ctx).Warnf("ignored xattr %s in archive", key)
					continue
				}
				return fmt.Errorf("failed to setxattr %q for key %q: %w", path, key, err)
			}
		}
	}

	// chmod after chown (chown may clear setuid/setgid bits); fchmod is
	// not umasked by the kernel, so no umask compensation is needed.
	if err := lchmodAt(fd, path, mode); err != nil {
		return err
	}

	atime := boundTime(latestTime(hdr.AccessTime, hdr.ModTime))
	mtime := boundTime(hdr.ModTime)
	return chtimesAt(fd, path, atime.UnixNano(), mtime.UnixNano())
}

// hasPathPrefix reports whether path is root itself or lies under root.
func hasPathPrefix(path, root string) bool {
	if path == root {
		return true
	}
	return len(path) > len(root) && path[:len(root)] == root && os.IsPathSeparator(path[len(root)])
}

func createTarFile(ctx context.Context, path, extractDir string, hdr *tar.Header, reader io.Reader, noSameOwner bool, dcache *dirfdCache) error {
	// hdr.Mode is in linux format, which we can use for syscalls,
	// but for os.Foo() calls we need the mode converted to os.FileMode,
	// so use hdrInfo.Mode() (they differ for e.g. setuid bits)
	hdrInfo := hdr.FileInfo()

	switch hdr.Typeflag {
	case tar.TypeDir:
		// Create directory unless it exists as a directory already.
		// In that case we just want to merge the two
		if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
			if err := mkdir(path, hdrInfo.Mode()); err != nil {
				fmt.Fprintf(os.Stderr, "DBG mkdir path=%q ppath=%q base=%q name=%q\n", path, extractDir, filepath.Base(path), hdr.Name)
				return err
			}
		}

	//nolint:staticcheck // TypeRegA is deprecated but we may still receive an external tar with TypeRegA
	case tar.TypeReg, tar.TypeRegA:
		file, err := openFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdrInfo.Mode())
		if err != nil {
			return err
		}

		_, err = copyBuffered(ctx, file, reader)
		if err1 := file.Close(); err == nil {
			err = err1
		}
		if err != nil {
			return err
		}

	case tar.TypeBlock, tar.TypeChar:
		// Handle this is an OS-specific way
		if err := handleTarTypeBlockCharFifo(hdr, path); err != nil {
			return err
		}

	case tar.TypeFifo:
		// Handle this is an OS-specific way
		if err := handleTarTypeBlockCharFifo(hdr, path); err != nil {
			return err
		}

	case tar.TypeLink:
		targetPath, err := hardlinkRootPath(extractDir, hdr.Linkname)
		if err != nil {
			return err
		}

		// link(2) does not follow a symlink at the final component of
		// newpath, but the LEXICAL path may still traverse symlinks in
		// intermediate components whose raw targets point outside root
		// (absolute archive symlink names). Resolve the canonical parent
		// from the dirfd cache instead when available.
		if dcache != nil && dcache.openat2OK {
			if pfd, perr := dcache.get(relFromRoot(extractDir, filepath.Dir(path))); perr == nil {
				canonical, rerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", pfd))
				if rerr == nil {
					path = filepath.Join(canonical, filepath.Base(path))
				}
			}
		}
		if err := link(targetPath, path); err != nil {
			return err
		}

	case tar.TypeSymlink:
		if err := os.Symlink(hdr.Linkname, path); err != nil {
			return err
		}

	case tar.TypeXGlobalHeader:
		log.G(ctx).Debug("PAX Global Extended Headers found and ignored")
		return nil

	default:
		return fmt.Errorf("unhandled tar header type %d", hdr.Typeflag)
	}

	if !noSameOwner {
		if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil {
			err = fmt.Errorf("failed to Lchown %q for UID %d, GID %d: %w", path, hdr.Uid, hdr.Gid, err)
			if errors.Is(err, syscall.EINVAL) && userns.RunningInUserNS() {
				err = fmt.Errorf("%w (Hint: try increasing the number of subordinate IDs in /etc/subuid and /etc/subgid)", err)
			}
			return err
		}
	}

	for key, value := range hdr.PAXRecords {
		if strings.HasPrefix(key, paxSchilyXattr) {
			key = key[len(paxSchilyXattr):]
			if err := setxattr(path, key, value); err != nil {
				if errors.Is(err, syscall.EPERM) && strings.HasPrefix(key, userXattrPrefix) {
					// In the user.* namespace, only regular files and directories can have extended attributes.
					// See https://man7.org/linux/man-pages/man7/xattr.7.html for details.
					if fi, err := os.Lstat(path); err == nil && (!fi.Mode().IsRegular() && !fi.Mode().IsDir()) {
						log.G(ctx).WithError(err).Warnf("ignored xattr %s in archive", key)
						continue
					}
				}
				if errors.Is(err, syscall.ENOTSUP) {
					log.G(ctx).WithError(err).Warnf("ignored xattr %s in archive", key)
					continue
				}
				return fmt.Errorf("failed to setxattr %q for key %q: %w", path, key, err)
			}
		}
	}

	// call lchmod after lchown since lchown can modify the file mode
	if err := lchmod(path, hdrInfo.Mode()); err != nil {
		return err
	}

	return chtimes(path, boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime))
}

func mkparent(ctx context.Context, path, root string, parents []string) error {
	if dir, err := os.Lstat(path); err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{
			Op:   "mkparent",
			Path: path,
			Err:  syscall.ENOTDIR,
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	i := len(path)
	for i > len(root) && !os.IsPathSeparator(path[i-1]) {
		i--
	}

	if i > len(root)+1 {
		if err := mkparent(ctx, path[:i-1], root, parents); err != nil {
			return err
		}
	}

	if err := mkdir(path, 0755); err != nil {
		// Check that still doesn't exist
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}

	for _, p := range parents {
		ppath, err := fs.RootPath(p, path[len(root):])
		if err != nil {
			return err
		}

		dir, err := os.Lstat(ppath)
		if err == nil {
			if !dir.IsDir() {
				// Replaced, do not copy attributes
				break
			}
			if err := copyDirInfo(dir, path); err != nil {
				return err
			}
			return copyUpXAttrs(path, ppath)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	log.G(ctx).Debugf("parent directory %q not found: default permissions(0755) used", path)

	return nil
}

// mkparentCached is mkparent with an existence memo for every directory it
// proves (not just the leaf). Sibling entries under the same tree skip the
// whole Lstat walk, which otherwise recurses per level per entry.
func mkparentCached(ctx context.Context, path, root string, parents []string, memo *sync.Map) error {
	if _, ok := memo.Load(path); ok {
		return nil
	}
	if err := mkparent(ctx, path, root, parents); err != nil {
		return err
	}
	// path and every ancestor between root and path now exists as a dir.
	memo.Store(path, struct{}{})
	for i := len(path); i > len(root)+1; {
		for i > len(root) && !os.IsPathSeparator(path[i-1]) {
			i--
		}
		if i <= len(root)+1 {
			break
		}
		memo.Store(path[:i-1], struct{}{})
		i-- // step past the separator to length of the parent
	}
	return nil
}

// ChangeWriter provides tar stream from filesystem change information.
// The privided tar stream is styled as an OCI layer. Change information
// (add/modify/delete/unmodified) for each file needs to be passed to this
// writer through HandleChange method.
//
// This should be used combining with continuity's diff computing functionality
// (e.g. `fs.Change` of github.com/containerd/continuity/fs).
//
// See also https://github.com/opencontainers/image-spec/blob/main/layer.md for details
// about OCI layers
type ChangeWriter struct {
	tw                *tar.Writer
	source            string
	modTimeUpperBound *time.Time
	inodeSrc          map[uint64]string
	inodeRefs         map[uint64][]string
	addedDirs         map[string]struct{}
}

// ChangeWriterOpt can be specified in NewChangeWriter.
type ChangeWriterOpt func(cw *ChangeWriter)

// WithModTimeUpperBound sets the mod time upper bound.
func WithModTimeUpperBound(tm time.Time) ChangeWriterOpt {
	return func(cw *ChangeWriter) {
		cw.modTimeUpperBound = &tm
	}
}

// NewChangeWriter returns ChangeWriter that writes tar stream of the source directory
// to the privided writer. Change information (add/modify/delete/unmodified) for each
// file needs to be passed through HandleChange method.
func NewChangeWriter(w io.Writer, source string, opts ...ChangeWriterOpt) *ChangeWriter {
	cw := &ChangeWriter{
		tw:        tar.NewWriter(w),
		source:    source,
		inodeSrc:  map[uint64]string{},
		inodeRefs: map[uint64][]string{},
		addedDirs: map[string]struct{}{},
	}
	for _, o := range opts {
		o(cw)
	}
	return cw
}

// HandleChange receives filesystem change information and reflect that information to
// the result tar stream. This function implements `fs.ChangeFunc` of continuity
// (github.com/containerd/continuity/fs) and should be used with that package.
func (cw *ChangeWriter) HandleChange(k fs.ChangeKind, p string, f os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if k == fs.ChangeKindDelete {
		whiteOutDir := filepath.Dir(p)
		whiteOutBase := filepath.Base(p)
		whiteOut := filepath.Join(whiteOutDir, whiteoutPrefix+whiteOutBase)
		// Since containerd v2.0, the whiteout timestamps are set to zero (1970-01-01),
		// not to the source date epoch.
		whiteOutT := time.Unix(0, 0).UTC()
		hdr := &tar.Header{
			Typeflag:   tar.TypeReg,
			Name:       whiteOut[1:],
			Size:       0,
			ModTime:    whiteOutT,
			AccessTime: whiteOutT,
			ChangeTime: whiteOutT,
		}
		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write whiteout header: %w", err)
		}
	} else {
		var (
			link   string
			err    error
			source = filepath.Join(cw.source, p)
		)

		switch {
		case f.Mode()&os.ModeSocket != 0:
			return nil // ignore sockets
		case f.Mode()&os.ModeSymlink != 0:
			if link, err = os.Readlink(source); err != nil {
				return err
			}
		}

		// Use FileInfoHeaderNoLookups to avoid propagating user names and group names from the host
		hdr, err := tarheader.FileInfoHeaderNoLookups(f, link)
		if err != nil {
			return err
		}

		hdr.Mode = int64(chmodTarEntry(os.FileMode(hdr.Mode)))

		// truncate timestamp for compatibility. without PAX stdlib rounds timestamps instead
		hdr.Format = tar.FormatPAX
		if cw.modTimeUpperBound != nil && hdr.ModTime.After(*cw.modTimeUpperBound) {
			hdr.ModTime = *cw.modTimeUpperBound
		}
		hdr.ModTime = hdr.ModTime.Truncate(time.Second)
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}

		name := p
		if strings.HasPrefix(name, string(filepath.Separator)) {
			name, err = filepath.Rel(string(filepath.Separator), name)
			if err != nil {
				return fmt.Errorf("failed to make path relative: %w", err)
			}
		}
		// Canonicalize to POSIX-style paths using forward slashes. Directory
		// entries must end with a slash.
		name = filepath.ToSlash(name)
		if f.IsDir() && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr.Name = name

		if err := setHeaderForSpecialDevice(hdr, name, f); err != nil {
			return fmt.Errorf("failed to set device headers: %w", err)
		}

		// additionalLinks stores file names which must be linked to
		// this file when this file is added
		var additionalLinks []string
		inode, isHardlink := fs.GetLinkInfo(f)
		if isHardlink {
			// If the inode has a source, always link to it
			if source, ok := cw.inodeSrc[inode]; ok {
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = source
				hdr.Size = 0
			} else {
				if k == fs.ChangeKindUnmodified {
					cw.inodeRefs[inode] = append(cw.inodeRefs[inode], name)
					return nil
				}
				cw.inodeSrc[inode] = name
				additionalLinks = cw.inodeRefs[inode]
				delete(cw.inodeRefs, inode)
			}
		} else if k == fs.ChangeKindUnmodified {
			// Nothing to write to diff
			return nil
		}

		if capability, err := getxattr(source, "security.capability"); err != nil {
			return fmt.Errorf("failed to get capabilities xattr: %w", err)
		} else if len(capability) > 0 {
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords[paxSchilyXattr+"security.capability"] = string(capability)
		}

		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write file header: %w", err)
		}

		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			file, err := open(source)
			if err != nil {
				return fmt.Errorf("failed to open path: %v: %w", source, err)
			}
			defer file.Close()

			n, err := copyBuffered(context.TODO(), cw.tw, file)
			if err != nil {
				return fmt.Errorf("failed to copy: %v: %w", source, err)
			}
			if n != hdr.Size {
				return errors.New("short write copying file")
			}
		}

		if additionalLinks != nil {
			source = hdr.Name
			for _, extra := range additionalLinks {
				hdr.Name = extra
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = source
				hdr.Size = 0

				if err := cw.includeParents(hdr); err != nil {
					return err
				}
				if err := cw.tw.WriteHeader(hdr); err != nil {
					return fmt.Errorf("failed to write file header: %w", err)
				}
			}
		}
	}
	return nil
}

// Close closes this writer.
func (cw *ChangeWriter) Close() error {
	if err := cw.tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}
	return nil
}

func (cw *ChangeWriter) includeParents(hdr *tar.Header) error {
	if cw.addedDirs == nil {
		return nil
	}
	name := strings.TrimRight(hdr.Name, "/")
	fname := filepath.Join(cw.source, name)
	parent := filepath.Dir(name)
	pname := filepath.Join(cw.source, parent)

	// Do not include root directory as parent
	if fname != cw.source && pname != cw.source {
		_, ok := cw.addedDirs[parent]
		if !ok {
			cw.addedDirs[parent] = struct{}{}
			fi, err := os.Stat(pname)
			if err != nil {
				return err
			}
			if err := cw.HandleChange(fs.ChangeKindModify, parent, fi, nil); err != nil {
				return err
			}
		}
	}
	if hdr.Typeflag == tar.TypeDir {
		cw.addedDirs[name] = struct{}{}
	}
	return nil
}

func copyBuffered(ctx context.Context, dst io.Writer, src io.Reader) (written int64, err error) {
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)

	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			return
		default:
		}

		nr, er := src.Read(*buf)
		if nr > 0 {
			nw, ew := dst.Write((*buf)[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err

}

// hardlinkRootPath returns target linkname, evaluating and bounding any
// symlink to the parent directory.
//
// NOTE: Allow hardlink to the softlink, not the real one. For example,
//
//	touch /tmp/zzz
//	ln -s /tmp/zzz /tmp/xxx
//	ln /tmp/xxx /tmp/yyy
//
// /tmp/yyy should be softlink which be same of /tmp/xxx, not /tmp/zzz.
func hardlinkRootPath(root, linkname string) (string, error) {
	ppath, base := filepath.Split(linkname)
	ppath, err := fs.RootPath(root, ppath)
	if err != nil {
		return "", err
	}

	targetPath := filepath.Join(ppath, base)
	if !strings.HasPrefix(targetPath, root) {
		targetPath = root
	}
	return targetPath, nil
}

func validateWhiteout(path string) error {
	base := filepath.Base(path)
	dir := filepath.Dir(path)

	if base == whiteoutOpaqueDir {
		return nil
	}

	if strings.HasPrefix(base, whiteoutPrefix) {
		originalBase := base[len(whiteoutPrefix):]
		originalPath := filepath.Join(dir, originalBase)

		// Ensure originalPath is under dir
		if dir[len(dir)-1] != filepath.Separator {
			dir += string(filepath.Separator)
		}
		if !strings.HasPrefix(originalPath, dir) {
			return fmt.Errorf("invalid whiteout name: %v: %w", base, errInvalidArchive)
		}
	}
	return nil
}
