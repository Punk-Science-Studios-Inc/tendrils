// Package index is the device's local memory of the last state it synced for
// every path — the "base" that reconciliation compares against. It is a small
// embedded bbolt store (pure Go, no CGO) so it survives reboots and lets a
// device that was off for a week pick up exactly where it left off.
package index

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"ca.punkscience.tendrils/internal/tree"
)

var (
	entriesBucket = []byte("entries")
	metaBucket    = []byte("meta")
	retriesBucket = []byte("retries")
	mountsBucket  = []byte("mounts")
	lastReconcile = []byte("last-reconcile")
)

// Retry is a path's record of consecutive action failures: how many, and the
// earliest time worth trying again. It exists so one file the server will not
// accept stops being retried every pass — without it a permanently-rejected
// upload burns a slot and two log lines a minute, forever.
//
// Permanent marks a failure that repeating cannot fix (the server refused the
// blob outright rather than failing to receive it). Even those get a slow retry
// rather than being abandoned: what makes them permanent is the server's
// configuration, and that can change under us.
type Retry struct {
	Failures    int       `json:"failures"`
	NextAttempt time.Time `json:"next_attempt"`
	LastError   string    `json:"last_error"`
	Permanent   bool      `json:"permanent"`
}

// Store is the persistent index. Safe for use from the single sync engine.
type Store struct {
	db *bbolt.DB
}

// Open opens (creating if needed) the index database at path.
func Open(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{entriesBucket, metaBucket, retriesBucket, mountsBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("index: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database file.
func (s *Store) Close() error { return s.db.Close() }

// Put records the last-synced entry for its path, overwriting any prior one.
func (s *Store) Put(e *tree.Entry) error {
	if e == nil || e.Path == "" {
		return fmt.Errorf("index: put: entry has no path")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("index: marshal %s: %w", e.Path, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(entriesBucket).Put([]byte(e.Path), data)
	})
}

// Get returns the recorded entry for path, or nil if none is recorded.
func (s *Store) Get(path string) (*tree.Entry, error) {
	var e *tree.Entry
	err := s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(entriesBucket).Get([]byte(path))
		if data == nil {
			return nil
		}
		e = &tree.Entry{}
		return json.Unmarshal(data, e)
	})
	if err != nil {
		return nil, fmt.Errorf("index: get %s: %w", path, err)
	}
	return e, nil
}

// All returns every recorded entry keyed by path, tombstones included. The
// reconciler needs tombstones to tell a re-creation from a fresh file.
func (s *Store) All() (map[string]*tree.Entry, error) {
	out := make(map[string]*tree.Entry)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(entriesBucket).ForEach(func(k, v []byte) error {
			e := &tree.Entry{}
			if err := json.Unmarshal(v, e); err != nil {
				return err
			}
			out[string(k)] = e
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("index: all: %w", err)
	}
	return out, nil
}

// SetRetry records a path's failure state, overwriting any prior one.
func (s *Store) SetRetry(path string, r Retry) error {
	if path == "" {
		return fmt.Errorf("index: set retry: no path")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("index: marshal retry %s: %w", path, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(retriesBucket).Put([]byte(path), data)
	})
}

// ClearRetry forgets a path's failure state, called when its action succeeds so
// the backoff starts from scratch next time rather than from a stale count.
func (s *Store) ClearRetry(path string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(retriesBucket).Delete([]byte(path))
	})
}

// Retries returns every recorded failure state keyed by path. The engine reads
// them once per pass, like All, so planning costs one transaction rather than a
// lookup per path.
func (s *Store) Retries() (map[string]Retry, error) {
	out := make(map[string]Retry)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(retriesBucket).ForEach(func(k, v []byte) error {
			var r Retry
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out[string(k)] = r
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("index: retries: %w", err)
	}
	return out, nil
}

// SetLastReconcile records the time of the last successful reconcile, reported
// by the status command.
func (s *Store) SetLastReconcile(at time.Time) error {
	b, _ := at.MarshalBinary()
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Put(lastReconcile, b)
	})
}

// LastReconcile returns the recorded last-reconcile time; the zero time if none.
func (s *Store) LastReconcile() (time.Time, error) {
	var at time.Time
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(metaBucket).Get(lastReconcile)
		if v == nil {
			return nil
		}
		return at.UnmarshalBinary(v)
	})
	return at, err
}

// Mounts returns the subordinate mount boundaries recorded under the sync root,
// as root-relative directories. They are remembered so that an unmounted one,
// left behind as an empty directory, is still known to be a boundary.
func (s *Store) Mounts() ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(mountsBucket).ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	return out, err
}

// AddMounts records further mount boundaries; existing ones are kept.
func (s *Store) AddMounts(dirs []string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(mountsBucket)
		for _, d := range dirs {
			if err := b.Put([]byte(d), []byte{}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResetMounts forgets every recorded mount boundary. Adoption calls it, so a
// boundary the owner has removed for good stops holding its subtree back.
func (s *Store) ResetMounts() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(mountsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucket(mountsBucket)
		return err
	})
}
