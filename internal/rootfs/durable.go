package rootfs

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"ca.punkscience.tendrils/internal/syncpath"
)

// RenameAttempts and RenameBackoff bound how long a replacement waits out
// another process holding the destination open (Windows sharing violations).
// The wait is finite: past it the work stays pending for a later pass.
var (
	RenameAttempts = 6
	RenameBackoff  = 50 * time.Millisecond
)

// Promote renames the sealed staging file tmp over rel, refused with ErrChanged
// if rel no longer matches want, and flushes the directory so the rename
// survives a power cut where the platform allows it.
func (f *FS) Promote(tmp, rel string, want Expect) error {
	if err := f.Check(rel); err != nil {
		return err
	}
	if err := f.checkStaging(tmp, rel); err != nil {
		return err
	}
	return f.promote(tmp, rel, &want)
}

func (f *FS) promote(tmp, rel string, want *Expect) error {
	if err := f.Verify(); err != nil {
		return err
	}
	if err := f.confined(rel); err != nil {
		return err
	}
	if err := f.spelled(rel); err != nil {
		return err
	}
	err := f.whileShared(func() error {
		if want != nil {
			if err := f.unchanged(rel, *want); err != nil {
				return err
			}
		}
		return f.root.Rename(tmp, rel)
	})
	if err != nil {
		return err
	}
	return f.syncDir(path.Dir(rel))
}

// whileShared retries step while another process holds a file it needs, a
// bounded number of times. On Windows that lock fails the stat as well as the
// rename, so the check and the move are retried together.
func (f *FS) whileShared(step func() error) error {
	wait := RenameBackoff
	var err error
	for i := 0; i < RenameAttempts; i++ {
		if err = step(); err == nil || !isSharingViolation(err) {
			return err
		}
		time.Sleep(wait)
		wait *= 2
	}
	return fmt.Errorf("held open by another process: %w", err)
}

func (f *FS) syncDir(dir string) error {
	if !dirSyncSupported {
		return nil
	}
	d, err := f.root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// checkStaging confirms tmp is a staging file beside rel, not a name a signed
// event or a corrupt journal could point anywhere else.
func (f *FS) checkStaging(tmp, rel string) error {
	if path.Dir(tmp) != path.Dir(rel) || !strings.HasPrefix(path.Base(tmp), syncpath.TempPrefix) {
		return fmt.Errorf("%s: not a staging file for %s", tmp, rel)
	}
	return nil
}

// StagedInfo stats the staging file tmp for rel.
func (f *FS) StagedInfo(tmp, rel string) (fs.FileInfo, error) {
	if err := f.checkStaging(tmp, rel); err != nil {
		return nil, err
	}
	if err := f.confined(tmp); err != nil {
		return nil, err
	}
	return f.root.Lstat(tmp)
}

// Discard removes the staging file tmp. One already gone is not an error.
func (f *FS) Discard(tmp string) error {
	dir := path.Dir(tmp)
	if !strings.HasPrefix(path.Base(tmp), syncpath.TempPrefix) || syncpath.Reserved(dir, false) && dir != "." {
		return fmt.Errorf("%s: not a staging file", tmp)
	}
	if dir != "." {
		if err := f.Check(dir); err != nil {
			return err
		}
	}
	if err := f.confined(tmp); err != nil {
		return err
	}
	if err := f.root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// TrashName picks a free place in the trash for rel.
func (f *FS) TrashName(rel string) string {
	dst := syncpath.TrashDir + "/" + rel
	if _, err := f.root.Lstat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	return dst
}

// InTrash reports whether dst, a name from TrashName, exists.
func (f *FS) InTrash(dst string) (bool, error) {
	if !strings.HasPrefix(dst, syncpath.TrashDir+"/") {
		return false, fmt.Errorf("%s: not in the trash", dst)
	}
	_, err := f.root.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// TrashTo moves rel to dst, a name from TrashName, refused with ErrChanged if rel
// no longer matches want. It never replaces anything already at dst.
func (f *FS) TrashTo(rel, dst string, want Expect) error {
	if err := f.Check(rel); err != nil {
		return err
	}
	if !strings.HasPrefix(dst, syncpath.TrashDir+"/") {
		return fmt.Errorf("%s: not in the trash", dst)
	}
	return f.trashTo(rel, dst, &want)
}

func (f *FS) trashTo(rel, dst string, want *Expect) error {
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
		return &fs.PathError{Op: "trash", Path: dst, Err: fs.ErrExist}
	}
	err := f.whileShared(func() error {
		if want != nil {
			if err := f.unchanged(rel, *want); err != nil {
				return err
			}
		}
		return f.root.Rename(rel, dst)
	})
	if err != nil {
		return err
	}
	if err := f.syncDir(path.Dir(rel)); err != nil {
		return err
	}
	return f.syncDir(path.Dir(dst))
}
