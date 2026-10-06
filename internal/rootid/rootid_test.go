package rootid

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEstablishThenVerify(t *testing.T) {
	root := t.TempDir()
	id, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	if id.IsZero() {
		t.Fatal("established an empty identity")
	}
	if err := Verify(root, id); err != nil {
		t.Fatalf("verify own root: %v", err)
	}
}

func TestEstablishReusesExistingMarker(t *testing.T) {
	root := t.TempDir()
	first, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("re-establishing changed the id: %s → %s", first.ID, second.ID)
	}
}

func TestVerifyFailures(t *testing.T) {
	root := t.TempDir()
	id, err := Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	other := t.TempDir()
	if _, err := Establish(other); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		root string
		want Identity
		err  error
	}{
		{"not adopted", root, Identity{}, ErrNotAdopted},
		{"missing", filepath.Join(root, "gone"), id, ErrRootMissing},
		{"not a directory", file, id, ErrNotDirectory},
		{"empty mountpoint", empty, id, ErrNoMarker},
		{"another enrollment", other, id, ErrWrongRoot},
		{"another volume", root, Identity{ID: id.ID, FS: "linux-fsid:not-this-one"}, ErrWrongVolume},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err == ErrWrongVolume && id.FS == "" {
				t.Skip("no filesystem identity on this platform")
			}
			err := Verify(c.root, c.want)
			if !errors.Is(err, c.err) {
				t.Fatalf("Verify = %v, want %v", err, c.err)
			}
			var u *Unavailable
			if !errors.As(err, &u) {
				t.Fatalf("Verify error is %T, want *Unavailable", err)
			}
		})
	}
}

func TestCorruptMarkerIsNotTrusted(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, MarkerName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, Identity{ID: "x"}); err == nil {
		t.Fatal("verified a marker with no id")
	}
	if _, err := Establish(root); err == nil {
		t.Fatal("established over a corrupt marker instead of refusing")
	}
}

func TestFilesystemIDIsStable(t *testing.T) {
	root := t.TempDir()
	a, err := FilesystemID(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FilesystemID(filepath.Join(root, "."))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("filesystem id changed between calls: %q vs %q", a, b)
	}
}
