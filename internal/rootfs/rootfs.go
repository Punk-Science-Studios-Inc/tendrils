// Package rootfs is the only way sync code touches files under a sync root.
//
// Every operation takes a wire path (forward-slash, root-relative), checks it
// with syncpath for this platform, and resolves it through an os.Root handle, so
// no path — however crafted, and whatever symlink or junction has been swapped in
// above it — can reach outside the root. Mutations additionally refuse to pass
// through a parent that is not a real directory, and re-verify the root's
// identity first: a drive pulled or remounted mid-pass stops the write instead of
// landing it somewhere else.
//
// Open a FS per pass and close it after. A held directory handle keeps a drive
// busy, and the owner must still be able to unmount it between passes.
package rootfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/syncpath"
)

// ErrNotConfined is returned when a parent of the target is a symlink, junction
// or other non-directory, so following it could put bytes somewhere the scan
// never looked.
var ErrNotConfined = errors.New("parent is not a plain directory")

// ErrCaseMismatch is returned on a case-insensitive root when a component of
// the target already exists spelled differently, so writing would land on — or
// inside — a different name than the one being synced.
var ErrCaseMismatch = errors.New("exists here in a different case")

// ErrUnverified is returned by a mutation on a FS opened without an identity.
var ErrUnverified = errors.New("root identity not verified; read-only")

// FS is an open sync root.
type FS struct {
	path string
	root *os.Root
	id   rootid.Identity
	plat syncpath.Platform
}

// Open opens root read-only: reads are confined, mutations refuse.
func Open(root string) (*FS, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	f := &FS{path: root, root: r, plat: syncpath.Local()}
	f.probeCase()
	return f, nil
}

// OpenVerified opens root after checking it is the folder id describes. The
// returned FS may mutate, and re-checks id before every mutation.
func OpenVerified(root string, id rootid.Identity) (*FS, error) {
	if err := rootid.Verify(root, id); err != nil {
		return nil, err
	}
	f, err := Open(root)
	if err != nil {
		return nil, err
	}
	f.id = id
	if err := f.Verify(); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// probeCase asks the filesystem itself whether case matters, using the marker
// (or failing that, the default for the OS). Linux mounts exFAT and VFAT
// case-insensitively, and Windows can enable case sensitivity per directory.
func (f *FS) probeCase() {
	if _, err := f.root.Lstat(syncpath.MarkerName); err != nil {
		return
	}
	_, err := f.root.Lstat(strings.ToUpper(syncpath.MarkerName))
	f.plat.CaseInsensitive = err == nil
}

func (f *FS) Close() error { return f.root.Close() }

// Path is the root's local path, for messages.
func (f *FS) Path() string { return f.path }

// Platform is the naming rules this root applies.
func (f *FS) Platform() syncpath.Platform { return f.plat }

// SetPlatform overrides the probed naming rules. For tests that exercise Windows
// rules on Linux.
func (f *FS) SetPlatform(p syncpath.Platform) { f.plat = p }

// Check reports whether rel may be acted on under this root.
func (f *FS) Check(rel string) error { return syncpath.Check(rel, f.plat) }

// Verify confirms the root is still the enrolled folder and that the configured
// path still leads to the directory this handle holds open.
func (f *FS) Verify() error {
	if f.id.IsZero() {
		return ErrUnverified
	}
	if err := rootid.Verify(f.path, f.id); err != nil {
		return err
	}
	held, err := f.root.Stat(".")
	if err != nil {
		return &rootid.Unavailable{Root: f.path, Cause: err}
	}
	now, err := os.Stat(f.path)
	if err != nil {
		return &rootid.Unavailable{Root: f.path, Cause: err}
	}
	if !os.SameFile(held, now) {
		return &rootid.Unavailable{Root: f.path, Cause: errors.New("sync root was replaced during the pass")}
	}
	return nil
}

// FS exposes the root as an io/fs filesystem for walking. Entries are reported
// as they are (symlinks are not followed by fs.WalkDir).
func (f *FS) FS() fs.FS { return f.root.FS() }

// Lstat stats rel without following a final symlink.
func (f *FS) Lstat(rel string) (fs.FileInfo, error) {
	if err := f.Check(rel); err != nil {
		return nil, err
	}
	return f.root.Lstat(rel)
}

// Open opens rel for reading. It must be a regular file, not a link to one.
func (f *FS) Open(rel string) (*os.File, error) {
	if err := f.Check(rel); err != nil {
		return nil, err
	}
	return f.openRegular(rel)
}

func (f *FS) openRegular(rel string) (*os.File, error) {
	if err := f.confined(rel); err != nil {
		return nil, err
	}
	before, err := f.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", rel)
	}
	file, err := f.root.Open(rel)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, fmt.Errorf("%s: replaced while opening", rel)
	}
	return file, nil
}

// ReadFile reads the regular file rel.
func (f *FS) ReadFile(rel string) ([]byte, error) {
	file, err := f.Open(rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// confined fails if any existing parent of rel is not a plain directory. A
// missing parent is fine: MkdirAll creates real directories.
func (f *FS) confined(rel string) error {
	for i := 0; i < len(rel); i++ {
		if rel[i] != '/' {
			continue
		}
		info, err := f.root.Lstat(rel[:i])
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsDir() {
			return fmt.Errorf("%s: %w", rel[:i], ErrNotConfined)
		}
	}
	return nil
}

// spelled fails on a case-insensitive root if any existing component of rel is
// spelled differently on disk.
func (f *FS) spelled(rel string) error {
	if !f.plat.CaseInsensitive {
		return nil
	}
	parent := "."
	for _, name := range strings.Split(rel, "/") {
		entries, err := fs.ReadDir(f.root.FS(), parent)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			if strings.EqualFold(e.Name(), name) {
				if e.Name() != name {
					return fmt.Errorf("%s: %q %w", rel, e.Name(), ErrCaseMismatch)
				}
				found = true
				break
			}
		}
		if !found {
			return nil
		}
		parent = path.Join(parent, name)
	}
	return nil
}

// prepareParent verifies the root and creates rel's parent directories. The
// trash skips the spelling check: where a trashed file lands is not synced.
func (f *FS) prepareParent(rel string, checkSpelling bool) error {
	if err := f.Verify(); err != nil {
		return err
	}
	if err := f.confined(rel); err != nil {
		return err
	}
	if checkSpelling {
		if err := f.spelled(rel); err != nil {
			return err
		}
	}
	if dir := path.Dir(rel); dir != "." {
		if err := f.root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return f.confined(rel)
	}
	return nil
}

// Pending is a file being written beside its destination. Nothing at the
// destination changes until Commit; Abort discards it.
type Pending struct {
	f        *FS
	rel, tmp string
	file     *os.File
	closed   bool
}

// Create starts an atomic write of rel.
func (f *FS) Create(rel string) (*Pending, error) {
	if err := f.Check(rel); err != nil {
		return nil, err
	}
	return f.create(rel)
}

func (f *FS) create(rel string) (*Pending, error) {
	if err := f.prepareParent(rel, true); err != nil {
		return nil, err
	}
	for range 10 {
		tmp := path.Join(path.Dir(rel), syncpath.TempPrefix+randomSuffix())
		file, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &Pending{f: f, rel: rel, tmp: tmp, file: file}, nil
	}
	return nil, fmt.Errorf("%s: could not create a temp file", rel)
}

func randomSuffix() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (p *Pending) Write(b []byte) (int, error) { return p.file.Write(b) }

// Commit stamps the file's mtime (unless zero) and renames it over rel.
func (p *Pending) Commit(mtime time.Time) error {
	if err := p.close(); err != nil {
		return err
	}
	if !mtime.IsZero() {
		if err := p.f.root.Chtimes(p.tmp, time.Now(), mtime); err != nil {
			return err
		}
	}
	if err := p.f.Verify(); err != nil {
		return err
	}
	if err := p.f.confined(p.rel); err != nil {
		return err
	}
	if err := p.f.root.Rename(p.tmp, p.rel); err != nil {
		return err
	}
	p.tmp = ""
	return nil
}

// Abort discards an uncommitted write. It is a no-op after Commit.
func (p *Pending) Abort() {
	p.close()
	if p.tmp != "" {
		p.f.root.Remove(p.tmp)
	}
}

func (p *Pending) close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	return p.file.Close()
}

// WriteFile atomically replaces rel with data.
func (f *FS) WriteFile(rel string, data []byte, mtime time.Time) error {
	p, err := f.Create(rel)
	if err != nil {
		return err
	}
	defer p.Abort()
	if _, err := p.Write(data); err != nil {
		return err
	}
	return p.Commit(mtime)
}

// Trash moves rel under the root's trash directory, keeping its relative
// structure; a name already taken there gets a timestamp suffix.
func (f *FS) Trash(rel string) error {
	if err := f.Check(rel); err != nil {
		return err
	}
	dst := syncpath.TrashDir + "/" + rel
	if err := f.prepareParent(dst, false); err != nil {
		return err
	}
	if err := f.confined(rel); err != nil {
		return err
	}
	if err := f.spelled(rel); err != nil {
		return err
	}
	if _, err := f.root.Lstat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	return f.root.Rename(rel, dst)
}
