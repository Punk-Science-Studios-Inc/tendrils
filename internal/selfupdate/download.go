package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// downloadTimeout bounds fetching an archive. Generous: the reference fleet
// includes a Pi on a 72 Mbit link.
const downloadTimeout = 15 * time.Minute

// maxBinaryBytes caps one extracted binary. Two Go binaries with debug info
// stripped are tens of megabytes; this is room to spare, and a ceiling against
// a decompression bomb.
const maxBinaryBytes = 256 << 20

// ArchiveName is the release archive for one platform. It mirrors the
// name_template in .goreleaser.yaml — if that template changes, this must.
func ArchiveName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("tendrils_%s_%s_%s.%s", strings.TrimPrefix(version, "v"), goos, goarch, ext)
}

// BinaryName is a program's file name on a platform.
func BinaryName(program, goos string) string {
	if goos == "windows" {
		return program + ".exe"
	}
	return program
}

// Downloaded is a verified, unpacked release sitting in a staging directory.
// The staging directory is created *inside the destination directory* so the
// final move is a rename on the same filesystem — a rename either happens or it
// does not, which is the whole guarantee. Cleanup removes it.
type Downloaded struct {
	Dir      string            // staging directory (inside the destination)
	Archive  string            // the verified archive, still on disk
	SHA256   string            // its verified content hash
	Binaries map[string]string // program name -> staged path
}

// Cleanup removes the staging directory. Safe to call twice.
func (d *Downloaded) Cleanup() error {
	if d == nil || d.Dir == "" {
		return nil
	}
	return os.RemoveAll(d.Dir)
}

// Download fetches the release archive for one platform into a staging
// directory inside destDir, verifies it against the release's checksums.txt,
// and unpacks the requested programs.
//
// The order is load-bearing: the archive is verified before a single byte of it
// is unpacked, and unpacking writes only into the staging directory. Nothing
// this function does can touch the binary being replaced.
//
// Refusals, not warnings: a missing archive for this platform, a checksums.txt
// that does not list the archive, and a hash that does not match are all hard
// errors. An unverified binary is worse than a stale one.
func (c *Client) Download(ctx context.Context, rel Release, goos, goarch, destDir string, programs []string) (_ *Downloaded, err error) {
	ctx, cancel := withTimeout(ctx, downloadTimeout)
	defer cancel()

	name := ArchiveName(rel.Version, goos, goarch)
	asset, ok := rel.Asset(name)
	if !ok {
		return nil, fmt.Errorf("%w: release %s publishes no %s", ErrNoAsset, rel.Version, name)
	}
	sums, ok := rel.Asset(ChecksumsAsset)
	if !ok {
		return nil, fmt.Errorf("selfupdate: release %s publishes no %s, so the download cannot be verified", rel.Version, ChecksumsAsset)
	}

	// The checksums come first. If they cannot be had, there is no point
	// spending a hundred megabytes of someone's link on bytes that could never
	// be trusted.
	sumBody, err := c.getBytes(ctx, sums.URL, 1<<20)
	if err != nil {
		return nil, err
	}
	want, err := checksumFor(sumBody, name)
	if err != nil {
		return nil, err
	}

	// Staging inside the destination proves up front that the destination is
	// writable — better to fail here than after the download.
	dir, err := os.MkdirTemp(destDir, ".tendrils-upgrade-")
	if err != nil {
		return nil, fmt.Errorf("selfupdate: cannot stage an upgrade in %s: %w", destDir, err)
	}
	d := &Downloaded{Dir: dir, Binaries: map[string]string{}}
	defer func() {
		if err != nil {
			d.Cleanup()
		}
	}()

	d.Archive = filepath.Join(dir, name)
	got, n, err := c.fetchToFile(ctx, asset.URL, d.Archive)
	if err != nil {
		return nil, err
	}
	// A truncated body whose length the server declared is caught here as well
	// as by the hash — it names the failure properly instead of reporting a
	// mismatch that looks like tampering.
	if asset.Size > 0 && n != asset.Size {
		return nil, fmt.Errorf("selfupdate: %s downloaded %d bytes of %d — the transfer was cut short", name, n, asset.Size)
	}
	if got != want {
		return nil, fmt.Errorf("%w for %s: release lists %s, downloaded %s", ErrChecksumMismatch, name, want, got)
	}
	d.SHA256 = got

	wanted := map[string]string{} // file name in archive -> program name
	for _, p := range programs {
		wanted[BinaryName(p, goos)] = p
	}
	if err := unpack(d.Archive, goos, wanted, dir, d.Binaries); err != nil {
		return nil, err
	}
	return d, nil
}

// fetchToFile streams a URL to a file, hashing as it goes. Streaming rather
// than buffering is not incidental: the reference host is a 1.8 GB Pi, and
// io.ReadAll's grow-and-copy peaks at roughly twice the payload. Memory here is
// one 32 KB copy buffer regardless of release size.
func (c *Client) fetchToFile(ctx context.Context, url, dest string) (sum string, n int64, err error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("selfupdate: %w", err)
	}
	defer f.Close()

	limit := c.MaxArchiveBytes
	if limit <= 0 {
		limit = 512 << 20
	}
	h := sha256.New()
	w := bufio.NewWriter(f)
	n, err = io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", n, fmt.Errorf("selfupdate: download %s: %w", url, err)
	}
	if n > limit {
		return "", n, fmt.Errorf("selfupdate: %s is larger than the %d-byte ceiling", url, limit)
	}
	if err := w.Flush(); err != nil {
		return "", n, fmt.Errorf("selfupdate: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", n, fmt.Errorf("selfupdate: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// checksumFor finds one file's entry in a checksums.txt. An absent entry is
// ErrChecksumMissing and stops the upgrade — the same rule install.sh follows,
// for the same reason.
func checksumFor(body []byte, name string) (string, error) {
	s := bufio.NewScanner(bytes.NewReader(body))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 2 {
			continue
		}
		// GNU coreutils marks binary mode with a leading '*' on the file name.
		if strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 {
			return "", fmt.Errorf("selfupdate: %s lists %s with a malformed hash", ChecksumsAsset, name)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("selfupdate: %s lists %s with a malformed hash", ChecksumsAsset, name)
		}
		return sum, nil
	}
	if err := s.Err(); err != nil {
		return "", fmt.Errorf("selfupdate: read %s: %w", ChecksumsAsset, err)
	}
	return "", fmt.Errorf("%w: %s", ErrChecksumMissing, name)
}

// unpack extracts the wanted programs from a verified archive into dir.
//
// Entries are matched on base name and always written to dir/<base>, so a
// crafted "../../bin/sh" entry cannot escape — by construction, not by
// checking. The explicit reject below is belt and braces, and says so in logs.
func unpack(archive, goos string, wanted map[string]string, dir string, out map[string]string) error {
	extract := func(name string, r io.Reader, mode os.FileMode) error {
		program, ok := wanted[path.Base(filepath.ToSlash(name))]
		if !ok {
			return nil
		}
		if strings.Contains(name, "..") {
			return fmt.Errorf("selfupdate: archive entry %q escapes the staging directory", name)
		}
		dest := filepath.Join(dir, BinaryName(program, goos))
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o700)
		if err != nil {
			return fmt.Errorf("selfupdate: %w", err)
		}
		defer f.Close()
		n, err := io.Copy(f, io.LimitReader(r, maxBinaryBytes+1))
		if err != nil {
			return fmt.Errorf("selfupdate: unpack %s: %w", name, err)
		}
		if n > maxBinaryBytes {
			return fmt.Errorf("selfupdate: %s in the archive is larger than the %d-byte ceiling", name, maxBinaryBytes)
		}
		// The file is executed for verification before anything is renamed, so
		// it has to be on disk in full first.
		if err := f.Sync(); err != nil {
			return fmt.Errorf("selfupdate: %w", err)
		}
		out[program] = dest
		return nil
	}

	if goos == "windows" {
		return unpackZip(archive, extract)
	}
	return unpackTarGz(archive, extract)
}

func unpackTarGz(archive string, extract func(string, io.Reader, os.FileMode) error) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return fmt.Errorf("selfupdate: %s is not a gzip archive: %w", filepath.Base(archive), err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("selfupdate: read %s: %w", filepath.Base(archive), err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := extract(hdr.Name, tr, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
			return err
		}
	}
}

func unpackZip(archive string, extract func(string, io.Reader, os.FileMode) error) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("selfupdate: %s is not a zip archive: %w", filepath.Base(archive), err)
	}
	defer zr.Close()
	for _, e := range zr.File {
		if e.FileInfo().IsDir() {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return fmt.Errorf("selfupdate: read %s: %w", e.Name, err)
		}
		err = extract(e.Name, rc, e.Mode().Perm())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
