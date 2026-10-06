package index

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"ca.punkscience.tendrils/internal/tree"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := openTemp(t)
	mt := time.Unix(1700000000, 0)
	e := &tree.Entry{Path: "notes/a.md", Sha256: "deadbeef", Size: 42, ModTime: mt}

	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Sha256 != "deadbeef" || got.Size != 42 || !got.ModTime.Equal(mt) {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestGetMissingReturnsNil(t *testing.T) {
	s := openTemp(t)
	got, err := s.Get("nope")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for missing path, got %+v", got)
	}
}

func TestAllIncludesTombstones(t *testing.T) {
	s := openTemp(t)
	_ = s.Put(&tree.Entry{Path: "live.md", Sha256: "aa"})
	_ = s.Put(&tree.Entry{Path: "gone.md", Deleted: true})

	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if !all["gone.md"].Deleted {
		t.Errorf("tombstone lost its Deleted flag")
	}
}

func TestLastReconcilePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	at := time.Unix(1700001234, 0)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastReconcile(at); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.LastReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at) {
		t.Errorf("last reconcile = %v, want %v", got, at)
	}
}

func TestRetryRoundTripAndClear(t *testing.T) {
	s := openTemp(t)
	r := Retry{
		Failures:    3,
		NextAttempt: time.Unix(1700000600, 0),
		LastError:   "upload rejected (413)",
		Permanent:   true,
	}
	if err := s.SetRetry("big.flac", r); err != nil {
		t.Fatal(err)
	}

	all, err := s.Retries()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := all["big.flac"]
	if !ok {
		t.Fatal("retry record not returned")
	}
	if got.Failures != r.Failures || got.Permanent != r.Permanent || got.LastError != r.LastError {
		t.Errorf("got %+v, want %+v", got, r)
	}
	if !got.NextAttempt.Equal(r.NextAttempt) {
		t.Errorf("next attempt = %v, want %v", got.NextAttempt, r.NextAttempt)
	}

	if err := s.ClearRetry("big.flac"); err != nil {
		t.Fatal(err)
	}
	all, err = s.Retries()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("retries after clear = %v, want empty", all)
	}
}

// Retry state is per path and independent of the synced base, so recording a
// failure never disturbs what the reconciler compares against.
func TestRetryDoesNotTouchBase(t *testing.T) {
	s := openTemp(t)
	e := &tree.Entry{Path: "a.md", Sha256: "cafe", Size: 4, ModTime: time.Unix(1700000000, 0)}
	if err := s.Put(e); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetry("a.md", Retry{Failures: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("a.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Sha256 != "cafe" {
		t.Errorf("base entry changed: %+v", got)
	}
}

// An index written before retry state existed gains the bucket on open, so an
// upgrade does not have to migrate or rebuild it.
func TestRetriesOnIndexPredatingTheBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	old, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = old.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{entriesBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open pre-retry index: %v", err)
	}
	defer s.Close()

	all, err := s.Retries()
	if err != nil {
		t.Fatalf("reading retries from an upgraded index: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("retries = %v, want empty", all)
	}
	if err := s.SetRetry("a.md", Retry{Failures: 1}); err != nil {
		t.Errorf("writing to the new bucket: %v", err)
	}
}

func TestMountsPersistAndReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMounts([]string{"media", "backup/disk"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMounts([]string{"media"}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("mounts = %v, want two survivors of a reopen", got)
	}
	if err := s.ResetMounts(); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Mounts(); len(got) != 0 {
		t.Fatalf("mounts after reset = %v", got)
	}
}
