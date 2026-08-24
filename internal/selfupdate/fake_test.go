package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// A fake releases API, an httptest.Server, and real archives built in memory.
// Nothing in this package's tests touches the network or api.github.com: the
// endpoint is a field precisely so the whole update path — check, download,
// verify, unpack — can be driven deterministically, including the cases that
// only ever happen once (an empty repository) or that nobody can stage on
// purpose (a corrupted archive).

const testSlug = "test/tendrils"

type fakeRelease struct {
	tag        string
	draft      bool
	prerelease bool
	published  time.Time
	assets     map[string][]byte
	// declaredSize overrides the size reported in the API for one asset, so a
	// short transfer can be simulated without a flaky truncated connection.
	declaredSize map[string]int64
	// truncate serves fewer bytes than Content-Length promises.
	truncate map[string]bool
}

type fakeGitHub struct {
	t        *testing.T
	server   *httptest.Server
	releases []*fakeRelease
	// rateLimited makes every API request answer 403 with the header GitHub
	// sets when an anonymous client has used its sixty requests.
	rateLimited bool
	// forceLatest is served by /releases/latest whatever the sort order says,
	// so the endpoint can be made to answer with a specific release.
	forceLatest *fakeRelease
	// repoMissing makes the listing endpoint 404 too — a repository that does
	// not exist, or one that is private, seen anonymously.
	repoMissing bool
	requests    int
}

func newFakeGitHub(t *testing.T, releases ...*fakeRelease) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, releases: releases}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// client returns a Client pointed at the fake.
func (f *fakeGitHub) client() *Client {
	c := New()
	c.API = f.server.URL
	c.Slug = testSlug
	return c
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.requests++
	if f.rateLimited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/repos/"+testSlug+"/releases/latest":
		rel := f.latestStable()
		if rel == nil {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.writeJSON(w, f.toJSON(rel))
	case path == "/repos/"+testSlug+"/releases":
		if f.repoMissing {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		out := []any{}
		for _, rel := range f.sorted() {
			out = append(out, f.toJSON(rel))
		}
		f.writeJSON(w, out)
	case strings.HasPrefix(path, "/repos/"+testSlug+"/releases/tags/"):
		tag := strings.TrimPrefix(path, "/repos/"+testSlug+"/releases/tags/")
		for _, rel := range f.releases {
			if rel.tag == tag {
				f.writeJSON(w, f.toJSON(rel))
				return
			}
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	case strings.HasPrefix(path, "/download/"):
		parts := strings.SplitN(strings.TrimPrefix(path, "/download/"), "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		for _, rel := range f.releases {
			if rel.tag != parts[0] {
				continue
			}
			body, ok := rel.assets[parts[1]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if rel.truncate[parts[1]] {
				// Content-Length promises the whole asset; the connection dies
				// halfway. This is what a dropped link looks like to the client.
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.WriteHeader(http.StatusOK)
				w.Write(body[:len(body)/2])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				panic(http.ErrAbortHandler) // drop the connection mid-body
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(body)
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode fake response: %v", err)
	}
}

// toJSON renders one release the way the GitHub API does.
func (f *fakeGitHub) toJSON(rel *fakeRelease) map[string]any {
	assets := []map[string]any{}
	names := make([]string, 0, len(rel.assets))
	for name := range rel.assets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		size := int64(len(rel.assets[name]))
		if s, ok := rel.declaredSize[name]; ok {
			size = s
		}
		assets = append(assets, map[string]any{
			"name":                 name,
			"browser_download_url": f.server.URL + "/download/" + rel.tag + "/" + name,
			"size":                 size,
		})
	}
	published := rel.published
	if published.IsZero() {
		published = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	}
	return map[string]any{
		"tag_name":     rel.tag,
		"draft":        rel.draft,
		"prerelease":   rel.prerelease,
		"published_at": published.Format(time.RFC3339),
		"assets":       assets,
	}
}

// latestStable mirrors GitHub's /releases/latest, which excludes drafts and
// prereleases — and therefore 404s on a repository whose only releases are
// prereleases. That is not the same state as "no releases", and the code under
// test has to tell them apart.
func (f *fakeGitHub) latestStable() *fakeRelease {
	if f.forceLatest != nil {
		return f.forceLatest
	}
	for _, rel := range f.sorted() {
		if !rel.draft && !rel.prerelease {
			return rel
		}
	}
	return nil
}

func (f *fakeGitHub) sorted() []*fakeRelease {
	out := append([]*fakeRelease(nil), f.releases...)
	sort.Slice(out, func(i, j int) bool {
		a, aerr := parseSemver(out[i].tag)
		b, berr := parseSemver(out[j].tag)
		if aerr != nil || berr != nil {
			return out[i].tag > out[j].tag
		}
		return a.compare(b) > 0
	})
	return out
}

// --- archive construction -----------------------------------------------------

// release builds a fakeRelease with a real archive for one platform, the
// checksums.txt that verifies it, and optionally a release.json.
func release(t *testing.T, tag, goos, goarch string, binaries map[string]string, meta *ReleaseMeta) *fakeRelease {
	t.Helper()
	version := strings.TrimPrefix(tag, "v")
	name := ArchiveName(version, goos, goarch)

	files := map[string][]byte{}
	for program, content := range binaries {
		files[BinaryName(program, goos)] = []byte(content)
	}
	var archive []byte
	if goos == "windows" {
		archive = zipArchive(t, files)
	} else {
		archive = tarGzArchive(t, files)
	}

	rel := &fakeRelease{tag: tag, assets: map[string][]byte{
		name:           archive,
		ChecksumsAsset: checksums(map[string][]byte{name: archive}),
	}}
	if meta != nil {
		body, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		rel.assets[ReleaseMetaAsset] = body
	}
	return rel
}

func checksums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		// GoReleaser's format: hash, two spaces, file name.
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	return []byte(b.String())
}

func tarGzArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Something other than the binaries, as the real archive carries docs.
	all := map[string][]byte{"README.md": []byte("# Tendrils\n")}
	for k, v := range files {
		all[k] = v
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mode := int64(0o644)
		if name != "README.md" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(all[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(all[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
