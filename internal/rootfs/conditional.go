package rootfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"
)

// ErrChanged is returned when a destination no longer matches what the pass
// observed. Nothing was replaced; the next pass sees the new state and decides
// again.
var ErrChanged = errors.New("changed since it was observed")

// Expect is the state a conditional mutation requires at its destination: absent,
// or a regular file with this size and modification time. It is the same
// size+mtime identity the scan trusts, so a change that preserves both is missed
// here exactly as it would be there.
type Expect struct {
	Absent  bool
	Size    int64
	ModTime time.Time
}

// ExpectAbsent requires that nothing exists at the destination.
func ExpectAbsent() Expect { return Expect{Absent: true} }

func (want Expect) matches(info fs.FileInfo, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if want.Absent {
			return nil
		}
		return fmt.Errorf("removed: %w", ErrChanged)
	case err != nil:
		return err
	case want.Absent:
		return fmt.Errorf("created: %w", ErrChanged)
	case !info.Mode().IsRegular():
		return fmt.Errorf("no longer a regular file: %w", ErrChanged)
	case info.Size() != want.Size || !info.ModTime().Equal(want.ModTime):
		return fmt.Errorf("modified: %w", ErrChanged)
	}
	return nil
}

// Unchanged reports ErrChanged if rel no longer matches want.
func (f *FS) Unchanged(rel string, want Expect) error {
	if err := f.Check(rel); err != nil {
		return err
	}
	if err := f.confined(rel); err != nil {
		return err
	}
	info, err := f.root.Lstat(rel)
	return want.matches(info, err)
}

// CommitIf is Commit, refused with ErrChanged if rel no longer matches want.
// The check runs immediately before the rename. A writer that changes rel in the
// gap between the two is still replaced: no filesystem here offers a portable
// compare-and-swap rename, so the window is narrowed, not closed. On Windows a
// file another process holds open without delete sharing makes the rename fail,
// which leaves the original in place and the work pending.
func (p *Pending) CommitIf(mtime time.Time, want Expect) error {
	if err := p.f.Unchanged(p.rel, want); err != nil {
		return err
	}
	return p.Commit(mtime)
}

// CommitNew is Commit for a name that must not already exist. Where hard links
// are supported the link itself is the exclusive create; where they are not
// (FAT, exFAT) the name is checked and then renamed into, which a concurrent
// creator of the same name could still race.
func (p *Pending) CommitNew(mtime time.Time) error {
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
	if err := p.f.spelled(p.rel); err != nil {
		return err
	}
	err := p.f.root.Link(p.tmp, p.rel)
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if err == nil {
		p.f.root.Remove(p.tmp)
		p.tmp = ""
		return nil
	}
	if _, statErr := p.f.root.Lstat(p.rel); !errors.Is(statErr, fs.ErrNotExist) {
		return &fs.PathError{Op: "create", Path: p.rel, Err: fs.ErrExist}
	}
	if err := p.f.root.Rename(p.tmp, p.rel); err != nil {
		return err
	}
	p.tmp = ""
	return nil
}

// Preserve copies rel to a new sibling name, streaming through a fixed buffer so
// memory does not grow with the file. name is asked for a fresh destination
// until one is free. The source must match want before and after the copy, so a
// file rewritten mid-copy is never preserved half-old, half-new. On any failure
// the source is untouched and no partial copy is left under a synced name.
func (f *FS) Preserve(rel string, want Expect, name func() string) (string, error) {
	if err := f.Check(rel); err != nil {
		return "", err
	}
	if want.Absent {
		return "", errors.New("nothing to preserve")
	}
	src, err := f.openRegular(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("removed: %w", ErrChanged)
		}
		return "", err
	}
	defer src.Close()
	if err := want.matches(src.Stat()); err != nil {
		return "", err
	}

	for range 10 {
		dst := name()
		if path.Dir(dst) != path.Dir(rel) {
			return "", fmt.Errorf("%s: preservation must stay beside the original", dst)
		}
		if err := f.Check(dst); err != nil {
			return "", err
		}
		p, err := f.create(dst)
		if err != nil {
			return "", err
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			p.Abort()
			return "", err
		}
		n, err := io.Copy(p.file, src)
		if err == nil && n != want.Size {
			err = fmt.Errorf("copied %d of %d bytes: %w", n, want.Size, ErrChanged)
		}
		if err == nil {
			err = want.matches(src.Stat())
		}
		if err == nil {
			err = p.file.Sync()
		}
		if err == nil {
			err = p.CommitNew(time.Now())
		}
		p.Abort()
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("%s: no free preservation name", rel)
}

// TrashIf is Trash, refused with ErrChanged if rel no longer matches want.
func (f *FS) TrashIf(rel string, want Expect) error {
	if err := f.Unchanged(rel, want); err != nil {
		return err
	}
	return f.Trash(rel)
}
