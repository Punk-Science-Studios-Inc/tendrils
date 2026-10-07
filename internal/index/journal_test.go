package index

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"ca.punkscience.tendrils/internal/tree"
)

func TestOldIndexIsUpgradedInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bbolt.Tx) error {
		e, _ := tx.CreateBucket(entriesBucket)
		e.Put([]byte("a.md"), []byte(`{"Path":"a.md","Sha256":"aa"}`))
		e.Put([]byte("gone.md"), []byte(`{"Path":"gone.md","Deleted":true}`))
		r, _ := tx.CreateBucket(retriesBucket)
		r.Put([]byte("big.flac"), []byte(`{"failures":3,"permanent":true}`))
		return nil
	})
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open old index: %v", err)
	}
	defer s.Close()
	all, _ := s.All()
	if all["a.md"] == nil || all["a.md"].Sha256 != "aa" || all["gone.md"] == nil || !all["gone.md"].Deleted {
		t.Errorf("entries after upgrade = %v", all)
	}
	retries, _ := s.Retries()
	if r := retries["big.flac"]; r.Failures != 3 || !r.Permanent {
		t.Errorf("retry after upgrade = %+v", r)
	}
	if ops, err := s.Journal(); err != nil || len(ops) != 0 {
		t.Errorf("journal = %v, %v; want empty", ops, err)
	}
}

func TestNewerIndexIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bbolt.Tx) error {
		m, _ := tx.CreateBucket(metaBucket)
		return m.Put(schemaKey, []byte("99"))
	})
	db.Close()
	if s, err := Open(path); !errors.Is(err, ErrUnsupportedSchema) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open = %v, want ErrUnsupportedSchema", err)
	}
}

func TestCompleteRecordsBaseAndClearsJournalTogether(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	final := &tree.Entry{Path: "a.md", Sha256: "bb", ModTime: time.Unix(1, 0)}
	if err := s.Begin(Op{Kind: OpPull, Path: "a.md", Staged: ".tendrils-tmp-x", Final: final}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Get("a.md"); b != nil {
		t.Fatalf("base written at Begin: %+v", b)
	}
	if err := s.Complete("a.md", final); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Get("a.md"); b == nil || b.Sha256 != "bb" {
		t.Errorf("base = %+v", b)
	}
	if ops, _ := s.Journal(); len(ops) != 0 {
		t.Errorf("journal = %v", ops)
	}
}

func TestNewerJournalRecordIsHeldNotLost(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(journalBucket).Put([]byte("a.md"), []byte(`{"v":99,"kind":"teleport","staged":".tendrils-tmp-x"}`))
	})
	ops, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	op := ops["a.md"]
	if op.Kind != "" || op.Staged != ".tendrils-tmp-x" || op.Path != "a.md" {
		t.Errorf("op = %+v, want an uninterpretable record that still names its staging file", op)
	}
}
