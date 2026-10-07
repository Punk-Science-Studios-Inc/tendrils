package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/tree"
)

// land journals a verified download and puts it in place. Once the journal
// holds it, an interruption at any later point is finished by recover, and the
// staging file is kept for that.
func (e *Engine) land(tmp *rootfs.Pending, path string, remote *tree.Entry, want rootfs.Expect, conflictCopy bool) error {
	staged, err := tmp.Seal(remote.ModTime)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	e.fault("staged", path)
	op := index.Op{
		Kind:          index.OpPull,
		Path:          path,
		ExpectAbsent:  want.Absent,
		ExpectSize:    want.Size,
		ExpectModTime: want.ModTime,
		Staged:        staged.Path,
		StagedSize:    staged.Size,
		StagedModTime: staged.ModTime,
		Final:         remote,
		Started:       time.Now(),
	}
	if conflictCopy && !want.Absent {
		op.Preserve = conflictCopyPath(path, e.id.PublicHex(), time.Now())
	}
	if err := e.idx.Begin(op); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	tmp.Release()
	e.fault("journaled", path)
	return e.resume(op)
}

// deleteLocal moves the local file to the trash (recoverable for the retention
// window) and records the remote tombstone as the new base. A file changed since
// the scan is left for the next pass.
func (e *Engine) deleteLocal(path string, want rootfs.Expect) error {
	op := index.Op{
		Kind:          index.OpTrash,
		Path:          path,
		ExpectAbsent:  want.Absent,
		ExpectSize:    want.Size,
		ExpectModTime: want.ModTime,
		TrashTo:       e.fs.TrashName(path),
		Final:         &tree.Entry{Path: path, Deleted: true, ModTime: time.Now()},
		Started:       time.Now(),
	}
	if err := e.idx.Begin(op); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	e.fault("journaled", path)
	return e.resume(op)
}

// resume carries an operation forward from whatever state the disk is in. It is
// both the live path and the recovery path, so every step is safe to repeat:
// each one first looks for evidence it already happened.
func (e *Engine) resume(op index.Op) error {
	switch op.Kind {
	case index.OpPull:
		return e.resumePull(op)
	case index.OpTrash:
		return e.resumeTrash(op)
	default:
		return fmt.Errorf("journal record for %s (kind %q, version %d) is not understood by this build; left untouched", op.Path, op.Kind, op.V)
	}
}

func expectOf(op index.Op) rootfs.Expect {
	return rootfs.Expect{Absent: op.ExpectAbsent, Size: op.ExpectSize, ModTime: op.ExpectModTime}
}

func (e *Engine) resumePull(op index.Op) error {
	want := expectOf(op)
	info, err := e.fs.StagedInfo(op.Staged, op.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Either it was promoted before the interruption, or something removed it.
		// Only the destination's bytes can say which.
		sum, err := e.hashLocal(op.Path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && sum != op.Final.Sha256 {
			return e.abandon(op, "staged download is gone and the destination does not hold it")
		}
		if err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if info.Size() != op.StagedSize || !info.ModTime().Equal(op.StagedModTime) {
			return e.abandon(op, "staged download was altered")
		}
		if err := e.fs.Unchanged(op.Path, want); err != nil {
			return e.abandonIfChanged(op, err)
		}
		if op.Preserve != "" {
			if _, err := e.fs.Lstat(op.Preserve); errors.Is(err, fs.ErrNotExist) {
				if _, err := e.fs.Preserve(op.Path, want, func() string { return op.Preserve }); err != nil {
					return e.abandonIfChanged(op, fmt.Errorf("preserve conflict copy: %w", err))
				}
			} else if err != nil {
				return err
			}
			e.fault("preserved", op.Path)
		}
		if err := e.fs.Promote(op.Staged, op.Path, want); err != nil {
			return e.abandonIfChanged(op, fmt.Errorf("write: %w", err))
		}
		e.fault("promoted", op.Path)
	}
	return e.idx.Complete(op.Path, op.Final)
}

func (e *Engine) resumeTrash(op index.Op) error {
	trashed, err := e.fs.InTrash(op.TrashTo)
	if err != nil {
		return err
	}
	_, err = e.fs.Lstat(op.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && trashed:
	case errors.Is(err, fs.ErrNotExist):
		return e.abandon(op, "removed before it could be trashed")
	case err != nil:
		return err
	case trashed:
		return e.abandon(op, "trash destination already taken")
	default:
		if err := e.fs.TrashTo(op.Path, op.TrashTo, expectOf(op)); err != nil {
			return e.abandonIfChanged(op, fmt.Errorf("trash: %w", err))
		}
		e.fault("trashed", op.Path)
	}
	return e.idx.Complete(op.Path, op.Final)
}

// abandon drops an operation the disk no longer supports, keeping the base as
// it was, so the next pass decides the path from what is actually there.
func (e *Engine) abandon(op index.Op, why string) error {
	if op.Staged != "" {
		if err := e.fs.Discard(op.Staged); err != nil {
			return err
		}
	}
	if err := e.idx.Abandon(op.Path); err != nil {
		return err
	}
	return fmt.Errorf("%s: %w", why, rootfs.ErrChanged)
}

func (e *Engine) abandonIfChanged(op index.Op, err error) error {
	if errors.Is(err, rootfs.ErrChanged) {
		return e.abandon(op, err.Error())
	}
	return err
}

func (e *Engine) hashLocal(path string) (string, error) {
	f, err := e.fs.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// recover finishes operations an earlier run left outstanding, before anything
// is scanned. It returns the paths still outstanding afterwards; a pass leaves
// those alone. An unavailable root stops recovery, and the pass, outright.
func (e *Engine) recover(retries map[string]index.Retry, now time.Time) (map[string]bool, error) {
	ops, err := e.idx.Journal()
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool)
	for path, op := range ops {
		if retries[path].NextAttempt.After(now) {
			held[path] = true
			continue
		}
		e.log.Info("finishing interrupted operation", "path", path, "op", op.Kind, "started", op.Started)
		err := e.resume(op)
		var gone *rootid.Unavailable
		switch {
		case errors.As(err, &gone):
			return nil, err
		case errors.Is(err, rootfs.ErrChanged):
			e.log.Info("interrupted operation no longer applies; deciding again", "path", path, "err", err)
		case err != nil:
			held[path] = true
			e.log.Error("could not finish interrupted operation", "path", path, "err", err)
		}
		e.recordAttempt(path, retries[path], ignoreChanged(err), now)
	}
	return held, nil
}

func ignoreChanged(err error) error {
	if errors.Is(err, rootfs.ErrChanged) {
		return nil
	}
	return err
}

// discardOrphans removes staging files no journal record owns: downloads cut
// off before they were journaled. Only one process holds the index, so none of
// them can belong to a write still in progress.
func (e *Engine) discardOrphans(staged []string) {
	ops, err := e.idx.Journal()
	if err != nil {
		return
	}
	owned := make(map[string]bool, len(ops))
	for _, op := range ops {
		owned[op.Staged] = true
	}
	for _, tmp := range staged {
		if owned[tmp] {
			continue
		}
		if err := e.fs.Discard(tmp); err != nil {
			e.log.Warn("could not remove abandoned staging file", "path", tmp, "err", err)
			continue
		}
		e.log.Info("removed abandoned staging file", "path", tmp)
	}
}

// fault is a crash point for tests; nil in every real build.
func (e *Engine) fault(stage, path string) {
	if e.faultHook != nil {
		e.faultHook(stage, path)
	}
}
