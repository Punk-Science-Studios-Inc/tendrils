// Package scan walks the sync root and reports the current on-disk state as a
// map of path → *tree.Entry. It is how the engine learns "what the tree looks
// like now" to compare against the index (base) and the relay (remote).
//
// Content is addressed by the SHA-256 of the plaintext file bytes, matching the
// hash the reconciler and event codec use.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"ca.punkscience.tendrils/internal/ignore"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/syncpath"
	"ca.punkscience.tendrils/internal/tree"
)

// TrashDir is the sync-root-relative folder where deleted files are retained.
// It is Tendrils' own bookkeeping and is never itself synced.
const TrashDir = syncpath.TrashDir

// TempPrefix is the basename prefix of the temp files atomicWrite creates in the
// target's directory before renaming into place. They live inside the sync root,
// so scan must skip them by prefix: a crash between write and rename leaves one
// behind, and it must never be mistaken for a real file and published.
const TempPrefix = syncpath.TempPrefix

// ConflictMarker is embedded in the filename of a preserved losing version when
// two devices diverge. A conflict copy stays in the tree and syncs like any
// other file, so the owner sees it everywhere and can resolve it with a rename.
const ConflictMarker = ".tendrils-conflict-"

// IsConflictCopy reports whether a path is a preserved conflict copy.
func IsConflictCopy(path string) bool {
	return strings.Contains(path[strings.LastIndexByte(path, '/')+1:], ConflictMarker)
}

// Options shapes a scan.
type Options struct {
	// Base is the last-synced index, used as a size+mtime cache (nil hashes
	// everything). A file whose size and modification time still match its base
	// entry is assumed unchanged and its stored hash is reused instead of
	// re-reading the bytes — the difference between a scan that stats every file
	// and one that reads all of them. The tradeoff is the standard one: a change
	// that preserves both size and mtime is missed until one of them moves.
	Base map[string]*tree.Entry
	// Ignore is the node's effective ignore rules. Ignored files are never
	// opened or hashed, and a directory nothing beneath which can be re-included
	// is not even read.
	Ignore *ignore.Matcher
	// Mounts are subordinate mount boundaries seen on earlier scans. They are
	// reported unobserved whether or not anything is mounted there now, which is
	// what keeps an unmounted one — an empty directory — from reading as deleted.
	Mounts []string
}

// Result is what a scan saw, and what it could not see.
type Result struct {
	Entries  map[string]*tree.Entry
	Coverage Coverage
	// Blocked holds names on disk that cannot be synced from here — not valid
	// UTF-8, or a bookkeeping name in another case — with the reason. A blocked
	// directory blocks everything beneath it.
	Blocked map[string]string
}

// Gap is a root-relative path the scan could not observe: an unreadable
// directory, a file that could not be read, a subordinate mount, or an entry of
// a type Tendrils does not sync. It covers the path and everything beneath it.
type Gap struct {
	Path   string
	Reason string
}

// Coverage records where a scan's silence is not evidence. A path missing from
// Result.Entries only means "deleted" when Observed says it was looked at.
type Coverage struct {
	Gaps []Gap
	// Mounts lists every subordinate mount boundary known after this scan:
	// Options.Mounts plus any found now. Callers persist it.
	Mounts []string
	gaps   map[string]string
}

// Observed reports whether path, and every directory above it, was completely
// read — and therefore whether its absence may be taken as a deletion.
func (c Coverage) Observed(path string) bool {
	_, unseen := c.GapFor(path)
	return !unseen
}

// GapFor returns the gap covering path, if any.
func (c Coverage) GapFor(path string) (string, bool) {
	if len(c.gaps) == 0 {
		return "", false
	}
	if _, ok := c.gaps["."]; ok {
		return ".", true
	}
	for p := path; ; {
		if _, ok := c.gaps[p]; ok {
			return p, true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return "", false
		}
		p = p[:i]
	}
}

func (c *Coverage) add(path, reason string) {
	if c.gaps == nil {
		c.gaps = make(map[string]string)
	}
	if _, ok := c.gaps[path]; ok {
		return
	}
	c.gaps[path] = reason
	c.Gaps = append(c.Gaps, Gap{Path: path, Reason: reason})
}

// Tree walks root and returns a live Entry for every regular file it could read,
// keyed by forward-slash relative path, plus the coverage of that walk. Tendrils'
// own bookkeeping is skipped. Symlinks are not followed.
//
// A failure under the root does not fail the scan: it becomes a Gap, so one
// unreadable folder neither stalls the rest of the tree nor reads as a deletion.
// Only a root that cannot be read at all is an error.
func Tree(root string, opt Options) (Result, error) {
	fsys, err := rootfs.Open(root)
	if err != nil {
		return Result{}, fmt.Errorf("scan: %w", err)
	}
	defer fsys.Close()
	return Walk(fsys, opt)
}

// Walk is Tree over an already-open root. Every read goes through fsys, so a
// symlink or junction swapped in mid-walk cannot lead the scan outside the root.
func Walk(fsys *rootfs.FS, opt Options) (Result, error) {
	res := Result{Entries: make(map[string]*tree.Entry), Blocked: make(map[string]string)}
	cov := &res.Coverage
	known := make(map[string]bool, len(opt.Mounts))
	for _, m := range opt.Mounts {
		known[m] = true
		cov.Mounts = append(cov.Mounts, m)
		cov.add(m, "subordinate mount point")
	}
	rootInfo, err := fs.Stat(fsys.FS(), ".")
	if err != nil {
		return Result{}, fmt.Errorf("scan: %w", err)
	}

	err = fs.WalkDir(fsys.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			if rel == "." || d == nil {
				return err
			}
			cov.add(rel, err.Error())
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel != "." && Reserved(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if rel != "." {
			if cerr := fsys.Check(rel); cerr != nil {
				res.Blocked[rel] = cerr.Error()
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}

		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if opt.Ignore.PruneDir(rel) {
				return fs.SkipDir
			}
			if known[rel] {
				return fs.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				cov.add(rel, err.Error())
				return fs.SkipDir
			}
			if !sameDevice(rootInfo, info) {
				known[rel] = true
				cov.Mounts = append(cov.Mounts, rel)
				cov.add(rel, "subordinate mount point")
				return fs.SkipDir
			}
			return nil
		}
		if opt.Ignore.Match(rel) {
			return nil
		}
		if !d.Type().IsRegular() {
			cov.add(rel, "unsupported file type (symlink, junction or device)")
			return nil
		}

		info, err := d.Info()
		if err != nil {
			cov.add(rel, err.Error())
			return nil
		}
		if e := reuse(opt.Base[rel], rel, info); e != nil {
			res.Entries[rel] = e
			return nil
		}
		entry, err := hashFile(fsys, rel)
		if err != nil {
			cov.add(rel, err.Error())
			return nil
		}
		res.Entries[rel] = entry
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("scan: walk %s: %w", fsys.Path(), err)
	}
	return res, nil
}

// Reserved reports whether a root-relative path is Tendrils' own bookkeeping:
// the trash, an atomic-write temp file, or the root marker. Such a path is never
// scanned, published, pulled or deleted, whichever side names it.
func Reserved(rel string) bool { return syncpath.Reserved(rel, false) }

// reuse returns a scan entry taken from base when the on-disk file's size and
// mtime still match it, so its stored content hash can be trusted without
// re-reading the bytes. It returns nil when there is no live, hashed base entry
// or either attribute differs — the caller then hashes the file.
func reuse(base *tree.Entry, rel string, info fs.FileInfo) *tree.Entry {
	if base == nil || !base.Live() || base.Sha256 == "" {
		return nil
	}
	if base.Size != info.Size() || !base.ModTime.Equal(info.ModTime()) {
		return nil
	}
	return &tree.Entry{
		Path:    rel,
		Sha256:  base.Sha256,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
}

// HashFile computes the entry (path, sha256, size, mtime) for a single file.
func HashFile(root, rel string) (*tree.Entry, error) {
	fsys, err := rootfs.Open(root)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	defer fsys.Close()
	return hashFile(fsys, rel)
}

func hashFile(fsys *rootfs.FS, rel string) (*tree.Entry, error) {
	f, err := fsys.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("scan: open %s: %w", rel, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("scan: stat %s: %w", rel, err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("scan: hash %s: %w", rel, err)
	}

	return &tree.Entry{
		Path:    rel,
		Sha256:  hex.EncodeToString(h.Sum(nil)),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
}
