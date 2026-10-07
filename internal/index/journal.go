package index

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"ca.punkscience.tendrils/internal/tree"
)

// OpVersion is the journal record layout this build understands.
const OpVersion = 1

// Op kinds.
const (
	OpPull  = "pull"
	OpTrash = "trash"
)

// Op is an interrupted-or-in-flight mutation of one path. It is written before
// anything at the path changes, and removed in the same transaction that
// records Final as the path's base, so the index never certifies a file that
// was not put in place. At most one is outstanding per path.
type Op struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`
	Path string `json:"path"`

	// What the destination must still be for the operation to proceed: absent,
	// or a file of this size and mtime.
	ExpectAbsent  bool      `json:"expect_absent,omitempty"`
	ExpectSize    int64     `json:"expect_size,omitempty"`
	ExpectModTime time.Time `json:"expect_mtime,omitempty"`

	// Staged is the verified download beside the destination, as it was left on
	// disk (FAT rounds mtimes, so the stored ones are recorded, not the wanted).
	Staged        string    `json:"staged,omitempty"`
	StagedSize    int64     `json:"staged_size,omitempty"`
	StagedModTime time.Time `json:"staged_mtime,omitempty"`

	// Preserve is the conflict copy to make of the destination before replacing
	// it. Its name is fixed up front, so a retried operation never makes two.
	Preserve string `json:"preserve,omitempty"`

	// TrashTo is where a trashed file goes, fixed up front for the same reason.
	TrashTo string `json:"trash_to,omitempty"`

	// Final is the base recorded once the operation is complete.
	Final *tree.Entry `json:"final"`

	Started time.Time `json:"started"`
}

// Begin records op as outstanding.
func (s *Store) Begin(op Op) error {
	if op.Path == "" || op.Final == nil {
		return fmt.Errorf("index: begin: incomplete operation")
	}
	op.V = OpVersion
	data, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("index: marshal op %s: %w", op.Path, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(journalBucket).Put([]byte(op.Path), data)
	})
}

// Complete records final as path's base and clears its outstanding operation,
// atomically.
func (s *Store) Complete(path string, final *tree.Entry) error {
	data, err := json.Marshal(final)
	if err != nil {
		return fmt.Errorf("index: marshal %s: %w", path, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(entriesBucket).Put([]byte(path), data); err != nil {
			return err
		}
		return tx.Bucket(journalBucket).Delete([]byte(path))
	})
}

// Abandon clears path's outstanding operation without touching its base: the
// path is decided again from what is on disk.
func (s *Store) Abandon(path string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(journalBucket).Delete([]byte(path))
	})
}

// Journal returns every outstanding operation keyed by path. A record this
// build cannot interpret is returned with its Kind empty, so it is held, not
// lost, and whatever staging file it names is still protected.
func (s *Store) Journal() (map[string]Op, error) {
	out := make(map[string]Op)
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(journalBucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var op Op
			if err := json.Unmarshal(v, &op); err != nil || op.V > OpVersion {
				op.Kind = ""
				op.Path = string(k)
			}
			out[string(k)] = op
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("index: journal: %w", err)
	}
	return out, nil
}
