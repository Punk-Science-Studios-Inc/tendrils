// Package rootid gives a sync root an identity that survives the folder being
// unavailable, so "the folder is empty" can be told apart from "this is not the
// folder". An unmounted drive leaves its mountpoint behind as an empty
// directory, and a scan of that directory is indistinguishable from the owner
// deleting every file — which a sync tool then dutifully propagates to every
// other device.
//
// Two things identify a root, and both are local: they are never synced.
//
//   - A marker file at the root (MarkerName) holding a random ID. A missing
//     marker means the directory at the configured path is not the enrolled
//     folder: an empty mountpoint, a freshly created directory, the wrong drive.
//   - The filesystem's own identity (Linux f_fsid, Windows volume serial),
//     recorded at enrollment. A matching marker on a different filesystem is a
//     copy — a restored backup mounted in place — and a copy that is missing the
//     newer files would otherwise tombstone them everywhere.
//
// Either mismatch pauses work rather than guessing; `tendrils adopt` is the
// explicit way to vouch for a root again.
package rootid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ca.punkscience.tendrils/internal/syncpath"
)

// MarkerName is the root-relative name of the marker file.
const MarkerName = syncpath.MarkerName

// Identity is what enrollment records about a root.
type Identity struct {
	ID string `json:"id"`
	// FS is the filesystem identity; empty where the platform offers none, in
	// which case it is not compared.
	FS string `json:"fs,omitempty"`
}

// IsZero reports whether no identity has been recorded — an enrollment from a
// build that predates root identities, which must be adopted explicitly.
func (id Identity) IsZero() bool { return id.ID == "" }

type marker struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

// Sentinel causes, so callers can say what is wrong without parsing text.
var (
	ErrNotAdopted   = errors.New("sync root has no recorded identity")
	ErrRootMissing  = errors.New("sync root is missing")
	ErrNotDirectory = errors.New("sync root is not a directory")
	ErrNoMarker     = errors.New("sync root marker is missing")
	ErrWrongRoot    = errors.New("sync root marker belongs to a different enrollment")
	ErrWrongVolume  = errors.New("sync root is on a different filesystem than the one enrolled")
)

// Unavailable describes why a root cannot be trusted right now.
type Unavailable struct {
	Root  string
	Cause error
	Hint  string
}

func (u *Unavailable) Error() string {
	msg := fmt.Sprintf("%s: %v", u.Root, u.Cause)
	if u.Hint != "" {
		msg += " (" + u.Hint + ")"
	}
	return msg
}

func (u *Unavailable) Unwrap() error { return u.Cause }

// Verify checks that root is the folder want describes. It is cheap — a stat, a
// small read and a statfs — so it is run before every pass and every destructive
// action, not just at startup.
func Verify(root string, want Identity) error {
	if want.IsZero() {
		return &Unavailable{Root: root, Cause: ErrNotAdopted, Hint: "run 'tendrils adopt' to verify and adopt this folder"}
	}
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Unavailable{Root: root, Cause: ErrRootMissing, Hint: "is the drive holding it mounted?"}
		}
		return &Unavailable{Root: root, Cause: err}
	}
	if !info.IsDir() {
		return &Unavailable{Root: root, Cause: ErrNotDirectory}
	}
	m, err := readMarker(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Unavailable{Root: root, Cause: ErrNoMarker, Hint: "an unmounted drive leaves an empty mountpoint behind; mount it, or run 'tendrils adopt' if this really is the folder"}
		}
		return &Unavailable{Root: root, Cause: err}
	}
	if m.ID != want.ID {
		return &Unavailable{Root: root, Cause: ErrWrongRoot, Hint: "run 'tendrils adopt' if this really is the folder"}
	}
	if want.FS != "" {
		got, err := FilesystemID(root)
		if err != nil {
			return &Unavailable{Root: root, Cause: err}
		}
		if got != "" && got != want.FS {
			return &Unavailable{Root: root, Cause: ErrWrongVolume, Hint: fmt.Sprintf("enrolled %s, found %s; run 'tendrils adopt' if the folder was moved deliberately", want.FS, got)}
		}
	}
	return nil
}

// Establish writes a marker into root if it has none and returns the root's
// identity, reusing an existing marker's ID. It never removes anything. Whether
// root *should* be established is the caller's decision — see adopt and enroll.
func Establish(root string) (Identity, error) {
	m, err := readMarker(root)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		m, err = writeMarker(root)
		if err != nil {
			return Identity{}, err
		}
	default:
		return Identity{}, err
	}
	fs, err := FilesystemID(root)
	if err != nil {
		return Identity{}, err
	}
	return Identity{ID: m.ID, FS: fs}, nil
}

// HasMarker reports whether root carries a marker, and its ID if so.
func HasMarker(root string) (string, bool, error) {
	m, err := readMarker(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return m.ID, true, nil
}

func readMarker(root string) (marker, error) {
	data, err := os.ReadFile(filepath.Join(root, MarkerName))
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return marker{}, fmt.Errorf("rootid: parse %s: %w", MarkerName, err)
	}
	if strings.TrimSpace(m.ID) == "" {
		return marker{}, fmt.Errorf("rootid: %s has no id", MarkerName)
	}
	return m, nil
}

func writeMarker(root string) (marker, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return marker{}, fmt.Errorf("rootid: generate id: %w", err)
	}
	m := marker{ID: hex.EncodeToString(b[:]), Created: time.Now().UTC()}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return marker{}, err
	}
	tmp, err := os.CreateTemp(root, MarkerName+".tmp-*")
	if err != nil {
		return marker{}, fmt.Errorf("rootid: write marker: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return marker{}, fmt.Errorf("rootid: write marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return marker{}, fmt.Errorf("rootid: write marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return marker{}, fmt.Errorf("rootid: write marker: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(root, MarkerName)); err != nil {
		return marker{}, fmt.Errorf("rootid: write marker: %w", err)
	}
	return m, nil
}
