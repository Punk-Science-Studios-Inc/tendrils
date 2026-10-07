package rootfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/syncpath"
)

// enrolled returns an established root and, beside it, an "outside" directory
// whose contents must never change.
func enrolled(t *testing.T) (root, outside string, id rootid.Identity) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "vault")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := rootid.Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, outside, id
}

func open(t *testing.T, root string, id rootid.Identity) *FS {
	t.Helper()
	f, err := OpenVerified(root, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func assertOutsideUntouched(t *testing.T, outside string) {
	t.Helper()
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "victim.txt" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("outside directory changed: %v", names)
	}
	b, err := os.ReadFile(filepath.Join(outside, "victim.txt"))
	if err != nil || string(b) != "untouched" {
		t.Fatalf("victim changed: %q %v", b, err)
	}
}

func TestTraversalIsRejectedBeforeAnyWrite(t *testing.T) {
	root, outside, id := enrolled(t)
	f := open(t, root, id)
	for _, p := range []string{"../outside/victim.txt", "/etc/x", "a/../../outside/victim.txt", "C:/x", ".tendrils-root"} {
		if err := f.WriteFile(p, []byte("pwned"), time.Time{}); !errors.Is(err, syncpath.ErrInvalid) {
			t.Errorf("WriteFile(%q) = %v, want ErrInvalid", p, err)
		}
		if err := f.Trash(p); !errors.Is(err, syncpath.ErrInvalid) {
			t.Errorf("Trash(%q) = %v, want ErrInvalid", p, err)
		}
	}
	assertOutsideUntouched(t, outside)
}

func TestParentSymlinkCannotRedirectWrites(t *testing.T) {
	root, outside, id := enrolled(t)
	if err := os.Symlink(outside, filepath.Join(root, "notes")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := open(t, root, id)
	if err := f.WriteFile("notes/victim.txt", []byte("pwned"), time.Time{}); err == nil {
		t.Fatal("write through a parent symlink succeeded")
	}
	if err := f.Trash("notes/victim.txt"); err == nil {
		t.Fatal("trash through a parent symlink succeeded")
	}
	if _, err := f.ReadFile("notes/victim.txt"); err == nil {
		t.Fatal("read through a parent symlink succeeded")
	}
	assertOutsideUntouched(t, outside)
}

func TestParentSwappedAfterCreateIsRefusedAtCommit(t *testing.T) {
	root, outside, id := enrolled(t)
	f := open(t, root, id)
	p, err := f.Create("notes/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Abort()
	p.Write([]byte("data"))
	// Replace the real directory with a link to outside between create and commit.
	if err := os.Rename(filepath.Join(root, "notes"), filepath.Join(root, "moved")); err != nil {
		t.Skipf("directory holding an open file cannot be moved here (Windows): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "notes")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := p.Commit(time.Time{}); err == nil {
		t.Fatal("commit through a swapped-in symlink succeeded")
	}
	assertOutsideUntouched(t, outside)
}

func TestFinalSymlinkIsNotRead(t *testing.T) {
	root, outside, id := enrolled(t)
	if err := os.Symlink(filepath.Join(outside, "victim.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := open(t, root, id)
	if _, err := f.ReadFile("link.txt"); err == nil {
		t.Fatal("read followed a symlink")
	}
}

func TestReadOnlyRootRefusesMutation(t *testing.T) {
	root, _, _ := enrolled(t)
	f, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.WriteFile("a.md", []byte("x"), time.Time{}); !errors.Is(err, ErrUnverified) {
		t.Fatalf("write on unverified root = %v, want ErrUnverified", err)
	}
}

func TestReplacedRootStopsMutation(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	// Same marker, same filesystem, different directory: a copy put in place.
	marker, err := os.ReadFile(filepath.Join(root, rootid.MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+".old"); err != nil {
		t.Skipf("an open root cannot be moved here (Windows): %v", err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rootid.MarkerName), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	err = f.WriteFile("a.md", []byte("x"), time.Time{})
	var gone *rootid.Unavailable
	if !errors.As(err, &gone) {
		t.Fatalf("write after root replaced = %v, want rootid.Unavailable", err)
	}
	for _, d := range []string{root, root + ".old"} {
		if _, err := os.Stat(filepath.Join(d, "a.md")); err == nil {
			t.Fatalf("file written into %s", d)
		}
	}
}

func TestCaseMismatchIsRefusedOnCaseInsensitiveRoot(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	f.SetPlatform(syncpath.Platform{CaseInsensitive: true})
	if err := f.WriteFile("Docs/a.md", []byte("a"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("docs/b.md", []byte("b"), time.Time{}); !errors.Is(err, ErrCaseMismatch) {
		t.Fatalf("write into differently-cased dir = %v, want ErrCaseMismatch", err)
	}
	if err := f.WriteFile("Docs/A.md", []byte("A"), time.Time{}); !errors.Is(err, ErrCaseMismatch) {
		t.Fatalf("write over differently-cased file = %v, want ErrCaseMismatch", err)
	}
	if err := f.WriteFile("Docs/a.md", []byte("a2"), time.Time{}); err != nil {
		t.Fatalf("rewrite with exact spelling: %v", err)
	}
}

func TestAtomicWriteLeavesNoTempOnAbort(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	p, err := f.Create("dir/x.bin")
	if err != nil {
		t.Fatal(err)
	}
	p.Write([]byte("partial"))
	p.Abort()
	entries, err := os.ReadDir(filepath.Join(root, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("abort left %v behind", entries[0].Name())
	}
}

func TestUnicodeAndLongNamesRoundTrip(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	long := "música/日本語/ファイル-" + strings.Repeat("x", 200) + ".flac"
	mtime := time.Unix(1_700_000_000, 0)
	if err := f.WriteFile(long, []byte("audio"), mtime); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile(long)
	if err != nil || string(got) != "audio" {
		t.Fatalf("read back %q, %v", got, err)
	}
	info, err := f.Lstat(long)
	if err != nil || !info.ModTime().Equal(mtime) {
		t.Fatalf("mtime %v, %v", info, err)
	}
}
