package rootfs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ca.punkscience.tendrils/internal/syncpath"
)

func observe(t *testing.T, abs string) Expect {
	t.Helper()
	info, err := os.Lstat(abs)
	if err != nil {
		t.Fatal(err)
	}
	return Expect{Size: info.Size(), ModTime: info.ModTime()}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), syncpath.TempPrefix) {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestCommitIfRefusesAChangedDestination(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "a.md")
	os.WriteFile(abs, []byte("seen"), 0o600)
	want := observe(t, abs)
	os.WriteFile(abs, []byte("edited after"), 0o600)

	p, err := f.Create("a.md")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Abort()
	p.Write([]byte("incoming"))
	if err := p.CommitIf(time.Time{}, want); !errors.Is(err, ErrChanged) {
		t.Fatalf("CommitIf = %v, want ErrChanged", err)
	}
	p.Abort()
	if got, _ := os.ReadFile(abs); string(got) != "edited after" {
		t.Errorf("a.md = %q, want the edit kept", got)
	}
	assertNoTemps(t, root)
}

func TestCommitIfRefusesACreatedDestination(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	p, err := f.Create("new.md")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Abort()
	p.Write([]byte("incoming"))
	os.WriteFile(filepath.Join(root, "new.md"), []byte("made meanwhile"), 0o600)
	if err := p.CommitIf(time.Time{}, ExpectAbsent()); !errors.Is(err, ErrChanged) {
		t.Fatalf("CommitIf = %v, want ErrChanged", err)
	}
}

func TestPreserveNeverOverwritesAnExistingCopy(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "a.md")
	os.WriteFile(abs, []byte("loser"), 0o600)
	os.WriteFile(filepath.Join(root, "a.copy.md"), []byte("earlier loser"), 0o600)

	_, err := f.Preserve("a.md", observe(t, abs), func() string { return "a.copy.md" })
	if err == nil {
		t.Fatal("Preserve reused a taken name")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a.copy.md")); string(got) != "earlier loser" {
		t.Errorf("earlier copy = %q, was overwritten", got)
	}
	if got, _ := os.ReadFile(abs); string(got) != "loser" {
		t.Errorf("original = %q, want untouched", got)
	}
	assertNoTemps(t, root)
}

func TestPreserveTriesAnotherNameWhenTaken(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "a.md")
	os.WriteFile(abs, []byte("loser"), 0o600)
	os.WriteFile(filepath.Join(root, "a.1.md"), []byte("earlier"), 0o600)

	names := []string{"a.1.md", "a.2.md"}
	dst, err := f.Preserve("a.md", observe(t, abs), func() string { n := names[0]; names = names[1:]; return n })
	if err != nil {
		t.Fatal(err)
	}
	if dst != "a.2.md" {
		t.Errorf("preserved to %s, want a.2.md", dst)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a.2.md")); string(got) != "loser" {
		t.Errorf("copy = %q", got)
	}
}

func TestPreserveRefusesASourceThatChanged(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "a.md")
	os.WriteFile(abs, []byte("seen"), 0o600)
	want := observe(t, abs)
	os.WriteFile(abs, []byte("rewritten"), 0o600)

	if _, err := f.Preserve("a.md", want, func() string { return "a.copy.md" }); !errors.Is(err, ErrChanged) {
		t.Fatalf("Preserve = %v, want ErrChanged", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a.copy.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a copy was made of a source that no longer matched")
	}
	assertNoTemps(t, root)
}

func TestTrashIfRefusesAChangedFile(t *testing.T) {
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "a.md")
	os.WriteFile(abs, []byte("seen"), 0o600)
	want := observe(t, abs)
	os.WriteFile(abs, []byte("edited"), 0o600)

	if err := f.TrashIf("a.md", want); !errors.Is(err, ErrChanged) {
		t.Fatalf("TrashIf = %v, want ErrChanged", err)
	}
	if got, _ := os.ReadFile(abs); string(got) != "edited" {
		t.Errorf("a.md = %q, want it left in place", got)
	}
}

// A losing file is streamed, not loaded: copying 64 MiB must not allocate
// anything close to 64 MiB.
func TestPreserveAllocationIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 64 MiB")
	}
	root, _, id := enrolled(t)
	f := open(t, root, id)
	abs := filepath.Join(root, "big.bin")
	const size = 64 << 20
	if err := os.WriteFile(abs, bytes.Repeat([]byte{0xa5}, size), 0o600); err != nil {
		t.Fatal(err)
	}
	want := observe(t, abs)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	dst, err := f.Preserve("big.bin", want, func() string { return "big.copy.bin" })
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20 {
		t.Errorf("preserving %d bytes allocated %d", size, alloc)
	}
	if info, err := os.Stat(filepath.Join(root, dst)); err != nil || info.Size() != size {
		t.Errorf("copy = %v, %v; want %d bytes", info, err, size)
	}
}
