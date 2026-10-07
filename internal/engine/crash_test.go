package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"ca.punkscience.tendrils/internal/index"
	"ca.punkscience.tendrils/internal/keys"
	"ca.punkscience.tendrils/internal/rootfs"
	"ca.punkscience.tendrils/internal/rootid"
	"ca.punkscience.tendrils/internal/scan"
	"ca.punkscience.tendrils/internal/syncpath"
	"ca.punkscience.tendrils/internal/tree"
)

// crashFixture is everything a child process needs to run one pass against the
// same root, index and remote truth as its parent.
type crashFixture struct {
	Root   string
	Index  string
	Secret string
	RootID rootid.Identity
	Events []*nostr.Event
	Blobs  map[string][]byte
}

const crashExit = 75

// TestCrashChild is not a test on its own: the crash tests run this binary with
// it selected, and it exits mid-operation at the named fault point.
func TestCrashChild(t *testing.T) {
	path := os.Getenv("TENDRILS_CRASH_FIXTURE")
	if path == "" {
		t.Skip("run by the crash tests")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx crashFixture
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	id, err := keys.Parse(fx.Secret)
	if err != nil {
		t.Fatal(err)
	}
	ev, bl := newFakeEvents(), newFakeBlobs()
	for _, e := range fx.Events {
		ev.byPath[e.Tags.GetD()] = e
	}
	bl.data = fx.Blobs
	idx, err := index.Open(fx.Index)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(fx.Root, fx.RootID, id, idx, bl, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if at := os.Getenv("TENDRILS_CRASH_AT"); at != "" {
		eng.faultHook = func(stage, _ string) {
			if stage == at {
				os.Exit(crashExit)
			}
		}
	}
	if os.Getenv("TENDRILS_CRASH_RECOVER_ONLY") != "" {
		fsys, err := rootfs.OpenVerified(fx.Root, fx.RootID)
		if err != nil {
			t.Fatal(err)
		}
		eng.fs = fsys
		if _, err := eng.recover(nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		fsys.Close()
	} else if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	idx.Close()
}

// crashScenario is a device with a synced base and a remote change waiting.
type crashScenario struct {
	t     *testing.T
	root  string
	index string
	fx    string
}

func newCrashScenario(t *testing.T, setup func(eng *Engine, root string, id *keys.Identity, ev *fakeEvents, bl *fakeBlobs)) *crashScenario {
	t.Helper()
	id := mustID(t)
	ev, bl := newFakeEvents(), newFakeBlobs()
	root := t.TempDir()
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "index.db")
	idx, err := index.Open(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	rid, err := rootid.Establish(root)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(root, rid, id, idx, bl, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	setup(eng, root, id, ev, bl)
	idx.Close()

	fx := crashFixture{Root: root, Index: idxPath, Secret: id.SecretHex(), RootID: rid, Blobs: bl.data}
	for _, e := range ev.byPath {
		fx.Events = append(fx.Events, e)
	}
	data, err := json.Marshal(fx)
	if err != nil {
		t.Fatal(err)
	}
	fxPath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fxPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return &crashScenario{t: t, root: root, index: idxPath, fx: fxPath}
}

// run starts a child pass that exits at stage (none if empty), and reports
// whether it crashed there.
func (c *crashScenario) run(stage string, env ...string) bool {
	c.t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), "TENDRILS_CRASH_FIXTURE="+c.fx, "TENDRILS_CRASH_AT="+stage), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == crashExit {
		return true
	}
	if err != nil {
		c.t.Fatalf("child pass (fault %q) failed: %v\n%s", stage, err, out)
	}
	return false
}

// settle crashes at stage, crashes again if recovery reaches it again, runs
// recovery alone and hands the index to recovered, then lets a clean pass run
// and returns the index for inspection.
func (c *crashScenario) settle(stage string, recovered func(*index.Store)) *index.Store {
	c.t.Helper()
	if !c.run(stage) {
		c.t.Fatalf("fault point %q was never reached", stage)
	}
	c.run(stage)
	if c.run("", "TENDRILS_CRASH_RECOVER_ONLY=1") {
		c.t.Fatal("recovery crashed")
	}
	if recovered != nil {
		idx, err := index.Open(c.index)
		if err != nil {
			c.t.Fatal(err)
		}
		if ops, _ := idx.Journal(); len(ops) != 0 {
			c.t.Errorf("journal still holds %v after recovery", ops)
		}
		recovered(idx)
		idx.Close()
	}
	if c.run("") {
		c.t.Fatal("clean pass crashed")
	}
	idx, err := index.Open(c.index)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { idx.Close() })
	ops, err := idx.Journal()
	if err != nil {
		c.t.Fatal(err)
	}
	if len(ops) != 0 {
		c.t.Errorf("journal still holds %v after a clean pass", ops)
	}
	c.assertNoStaging()
	return idx
}

func (c *crashScenario) assertNoStaging() { c.t.Helper(); assertNoStagingIn(c.t, c.root) }

func assertNoStagingIn(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), syncpath.TempPrefix) {
			t.Errorf("staging file %s left behind", p)
		}
		return nil
	})
}

func assertBase(t *testing.T, idx *index.Store, path, content string) {
	t.Helper()
	b, err := idx.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Deleted || b.Sha256 != hashHex([]byte(content)) {
		t.Errorf("base for %s = %+v, want %q", path, b, content)
	}
}

func pullScenario(t *testing.T, remote string, chunked bool) func(*Engine, string, *keys.Identity, *fakeEvents, *fakeBlobs) {
	return func(eng *Engine, root string, id *keys.Identity, ev *fakeEvents, bl *fakeBlobs) {
		writeFile(t, root, "f.md", "v1", t0)
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		other := t.TempDir()
		writeFile(t, other, "f.md", remote, t0.Add(20*time.Second))
		a := newEngine(t, other, id, ev, bl)
		if chunked {
			a.chunkSize = 1000
		}
		if err := a.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCrashDuringPullRecovers(t *testing.T) {
	cases := map[string]struct {
		remote  string
		chunked bool
	}{
		"single":  {"v2 from the laptop", false},
		"chunked": {bigContent(5000), true},
	}
	for name, tc := range cases {
		for _, stage := range []string{"staged", "journaled", "promoted"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				c := newCrashScenario(t, pullScenario(t, tc.remote, tc.chunked))
				idx := c.settle(stage, func(idx *index.Store) {
					want := tc.remote
					if stage == "staged" {
						want = "v1"
					}
					assertBase(t, idx, "f.md", want)
				})
				if got, _ := readFile(t, c.root, "f.md"); got != tc.remote {
					t.Errorf("f.md = %.40q, want the remote version", got)
				}
				assertBase(t, idx, "f.md", tc.remote)
				if copies := conflictCopies(t, c.root, "f.md"); len(copies) != 0 {
					t.Errorf("unexpected conflict copies %v", copies)
				}
			})
		}
	}
}

func TestCrashDuringConflictPreservationRecovers(t *testing.T) {
	for _, stage := range []string{"staged", "journaled", "preserved", "promoted"} {
		t.Run(stage, func(t *testing.T) {
			c := newCrashScenario(t, func(eng *Engine, root string, id *keys.Identity, ev *fakeEvents, bl *fakeBlobs) {
				writeFile(t, root, "f.md", "v1", t0)
				if err := eng.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "f.md", "local loser", t0.Add(10*time.Second))
				seedRemote(t, ev, bl, id, "f.md", "remote winner", t0.Add(20*time.Second))
			})
			idx := c.settle(stage, func(idx *index.Store) {
				if stage == "staged" {
					assertBase(t, idx, "f.md", "v1")
					return
				}
				assertBase(t, idx, "f.md", "remote winner")
				if got, _ := readFile(t, c.root, "f.md"); got != "remote winner" {
					t.Errorf("recovery left f.md = %q", got)
				}
			})
			if got, _ := readFile(t, c.root, "f.md"); got != "remote winner" {
				t.Errorf("f.md = %q, want the remote winner", got)
			}
			if got, _ := readFile(t, c.root, onlyConflictCopy(t, c.root, "f.md")); got != "local loser" {
				t.Errorf("conflict copy = %q, want the local loser", got)
			}
			assertBase(t, idx, "f.md", "remote winner")
		})
	}
}

func TestCrashDuringTrashRecovers(t *testing.T) {
	for _, stage := range []string{"journaled", "trashed"} {
		t.Run(stage, func(t *testing.T) {
			c := newCrashScenario(t, func(eng *Engine, root string, id *keys.Identity, ev *fakeEvents, bl *fakeBlobs) {
				writeFile(t, root, "f.md", "v1", t0)
				if err := eng.Sync(context.Background()); err != nil {
					t.Fatal(err)
				}
				tomb := signEntryAt(t, id, &tree.Entry{Path: "f.md", Deleted: true, ModTime: t0.Add(20 * time.Second)}, time.Now().Unix()+10)
				ev.byPath["f.md"] = tomb
			})
			idx := c.settle(stage, func(idx *index.Store) {
				if b, _ := idx.Get("f.md"); b == nil || !b.Deleted {
					t.Errorf("recovery left base = %+v, want a tombstone", b)
				}
			})
			if _, ok := readFile(t, c.root, "f.md"); ok {
				t.Error("f.md still present after its deletion was recovered")
			}
			trashed, _ := filepath.Glob(filepath.Join(c.root, scan.TrashDir, "f.md*"))
			if len(trashed) != 1 {
				t.Fatalf("trash holds %v, want exactly one copy", trashed)
			}
			if got, _ := os.ReadFile(trashed[0]); string(got) != "v1" {
				t.Errorf("trashed copy = %q, want v1", got)
			}
			if b, _ := idx.Get("f.md"); b == nil || !b.Deleted {
				t.Errorf("base = %+v, want a tombstone", b)
			}
		})
	}
}
