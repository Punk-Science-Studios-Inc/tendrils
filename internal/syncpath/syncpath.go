// Package syncpath decides whether a wire path — the forward-slash, root-relative
// name every event, index row and scan result carries — may be acted on here.
//
// It answers two different questions and keeps them apart:
//
//   - Invalid: the path is not a canonical relative path on any platform
//     (absolute, drive or UNC prefix, traversal, empty segments, NUL, invalid
//     UTF-8) or it names Tendrils' own bookkeeping. No honest client produces
//     one, and nothing may be mutated for it anywhere.
//   - Blocked: the path is well formed but this platform cannot represent it as
//     one distinct local file — a Windows reserved name, a trailing dot or space,
//     an alternate-stream colon, an over-long component. Other devices keep
//     syncing it; this one leaves it untouched and says so.
//
// It performs no I/O.
package syncpath

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Bookkeeping names inside a sync root. They are defined here, the leaf, so the
// scanner, the engine and the root marker all agree on them.
const (
	TrashDir   = ".tendrils-trash"
	TempPrefix = ".tendrils-tmp-"
	MarkerName = ".tendrils-root"
)

// MaxComponent is the longest single path component: 255 bytes on Linux
// filesystems, 255 UTF-16 code units on NTFS.
const MaxComponent = 255

// Platform describes the local filesystem's naming rules.
type Platform struct {
	// Windows applies Win32 naming: reserved device names, forbidden characters,
	// trailing dots and spaces, 8.3 short-name aliases, UTF-16 length.
	Windows bool
	// CaseInsensitive means two names differing only in case are one file.
	CaseInsensitive bool
}

// Local is the running platform. CaseInsensitive is a default only; a root's
// real behaviour is probed by rootfs, since Linux can mount exFAT and Windows
// can enable per-directory case sensitivity.
func Local() Platform {
	w := runtime.GOOS == "windows"
	return Platform{Windows: w, CaseInsensitive: w}
}

var (
	ErrInvalid = errors.New("not a canonical relative sync path")
	ErrBlocked = errors.New("cannot be represented on this platform")
)

// Problem explains why a path cannot be acted on. It wraps ErrInvalid or
// ErrBlocked so callers can tell "never valid" from "not valid here".
type Problem struct {
	Path   string
	Reason string
	kind   error
}

func (p *Problem) Error() string { return fmt.Sprintf("%q %v: %s", p.Path, p.kind, p.Reason) }
func (p *Problem) Unwrap() error { return p.kind }

// Blocked reports whether err is a platform-representation problem rather than
// an invalid path.
func Blocked(err error) bool { return errors.Is(err, ErrBlocked) }

func invalid(path, reason string) error {
	return &Problem{Path: path, Reason: reason, kind: ErrInvalid}
}

func blocked(path, reason string) error {
	return &Problem{Path: path, Reason: reason, kind: ErrBlocked}
}

// Check reports whether path may be read, written, trashed or published on p.
func Check(path string, p Platform) error {
	if err := Canonical(path); err != nil {
		return err
	}
	if Reserved(path, p.CaseInsensitive) {
		return invalid(path, "names Tendrils bookkeeping")
	}
	for _, seg := range strings.Split(path, "/") {
		if err := checkSegment(path, seg, p); err != nil {
			return err
		}
	}
	return nil
}

// Canonical checks the platform-independent wire syntax: a non-empty relative
// path of non-empty forward-slash segments, none of them "." or "..".
func Canonical(path string) error {
	switch {
	case path == "":
		return invalid(path, "empty")
	case !utf8.ValidString(path):
		return invalid(path, "not valid UTF-8")
	case strings.IndexByte(path, 0) >= 0:
		return invalid(path, "contains NUL")
	case path[0] == '/':
		return invalid(path, "absolute")
	case hasDrive(path):
		return invalid(path, "drive-qualified")
	}
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "":
			return invalid(path, "empty segment")
		case ".", "..":
			return invalid(path, "traversal segment")
		}
	}
	return nil
}

func hasDrive(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	c := path[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// Reserved reports whether path is Tendrils' own bookkeeping: the trash, an
// atomic-write temp name in any component, or the root marker. Scan prunes all
// of these, so a path under one would read as deleted on the next pass. On a
// case-insensitive root a differently-cased spelling names the same file.
func Reserved(path string, caseInsensitive bool) bool {
	p := path
	if caseInsensitive {
		p = strings.ToLower(path)
	}
	if p == TrashDir || strings.HasPrefix(p, TrashDir+"/") {
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, TempPrefix) {
			return true
		}
	}
	return p == MarkerName || strings.HasPrefix(p, MarkerName+".tmp-")
}

func checkSegment(path, seg string, p Platform) error {
	if !p.Windows {
		if len(seg) > MaxComponent {
			return blocked(path, fmt.Sprintf("component longer than %d bytes", MaxComponent))
		}
		return nil
	}
	if n := len(utf16.Encode([]rune(seg))); n > MaxComponent {
		return blocked(path, fmt.Sprintf("component longer than %d UTF-16 units", MaxComponent))
	}
	for _, r := range seg {
		switch {
		case r < 0x20:
			return blocked(path, "control character in name")
		case r == ':':
			return blocked(path, "colon (alternate data stream syntax)")
		case strings.ContainsRune(`<>"|?*\`, r):
			return blocked(path, fmt.Sprintf("character %q is not allowed on Windows", r))
		}
	}
	if last := seg[len(seg)-1]; last == '.' || last == ' ' {
		return blocked(path, "trailing dot or space is dropped by Windows")
	}
	if reservedDevice(seg) {
		return blocked(path, "reserved Windows device name")
	}
	if shortNameAlias(seg) {
		return blocked(path, "looks like an 8.3 short name and may alias another file")
	}
	return nil
}

var devices = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

func reservedDevice(seg string) bool {
	stem := seg
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	if devices[stem] {
		return true
	}
	if len(stem) < 4 || (stem[:3] != "COM" && stem[:3] != "LPT") {
		return false
	}
	switch stem[3:] {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}

// shortNameAlias matches the 8.3 shape Windows generates (PROGRA~1, REPORT~2.TXT),
// which can resolve to an existing long-named file instead of creating a new one.
func shortNameAlias(seg string) bool {
	stem, ext, _ := strings.Cut(seg, ".")
	if strings.Contains(ext, ".") || len(ext) > 3 || len(stem) > 8 {
		return false
	}
	i := strings.LastIndexByte(stem, '~')
	if i < 1 || i == len(stem)-1 {
		return false
	}
	for _, c := range stem[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// FoldKey is the identity of path on a case-insensitive filesystem.
func FoldKey(path string) string { return strings.ToLower(path) }

// Collisions returns, for a case-insensitive filesystem, every path in names
// that shares a folded spelling — of the whole path or of any parent directory —
// with a differently-spelled path in names. Each would land on another's file or
// directory, so none of them can be synced here until the names differ.
func Collisions(names []string) map[string]string {
	spellings := make(map[string]map[string]struct{})
	for _, n := range names {
		for _, pre := range prefixes(n) {
			k := FoldKey(pre)
			if spellings[k] == nil {
				spellings[k] = make(map[string]struct{})
			}
			spellings[k][pre] = struct{}{}
		}
	}
	out := make(map[string]string)
	for _, n := range names {
		for _, pre := range prefixes(n) {
			if s := spellings[FoldKey(pre)]; len(s) > 1 {
				out[n] = fmt.Sprintf("case collision: %q differs from another synced name only in case", pre)
				break
			}
		}
	}
	return out
}

func prefixes(path string) []string {
	var out []string
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			out = append(out, path[:i])
		}
	}
	return append(out, path)
}
