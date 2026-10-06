// Package ignore implements a .gitignore-style matcher for Tendrils' per-vault
// ignore file. The file (`.tendrilsignore` at the sync root) is itself a synced
// file, so one edit propagates to every device — the ignore rules are shared,
// not per-device configuration.
//
// Supported syntax (a practical subset of gitignore):
//   - blank lines and `# comments` are skipped
//   - `!pattern` negates (re-includes); the last matching rule wins
//   - a trailing `/` marks a directory (matches the dir and everything under it)
//   - a leading or interior `/` anchors the pattern to the sync root; a pattern
//     with no such slash matches at any depth
//   - `*`, `?`, `[...]` match within a single path segment; `**` matches across
//     any number of segments
//
// Not supported: gitignore's rule that a re-include cannot resurrect a file
// under an excluded directory. Matching is per-path and independent.
package ignore

import (
	"path"
	"strings"
)

// FileName is the sync-root-relative name of the ignore file.
const FileName = ".tendrilsignore"

// Matcher is a compiled set of ignore rules. The zero value (and a nil *Matcher)
// matches nothing, so callers can treat "no ignore file" as an empty matcher.
type Matcher struct {
	rules []rule
}

type rule struct {
	neg      bool     // leading '!': re-include
	dirOnly  bool     // trailing '/': matches directories (and their contents) only
	anchored bool     // has a leading/interior '/': matched from the root
	segs     []string // pattern split on '/'
}

// Compile parses ignore-file lines into a Matcher.
func Compile(lines []string) *Matcher {
	var rules []rule
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		neg := false
		if strings.HasPrefix(t, "!") {
			neg = true
			t = t[1:]
		}
		dirOnly := strings.HasSuffix(t, "/")
		t = strings.TrimSuffix(t, "/")
		anchored := strings.Contains(t, "/")
		t = strings.TrimPrefix(t, "/")
		if t == "" {
			continue
		}
		rules = append(rules, rule{neg: neg, dirOnly: dirOnly, anchored: anchored, segs: strings.Split(t, "/")})
	}
	return &Matcher{rules: rules}
}

// Match reports whether relPath (a forward-slash, sync-root-relative file path)
// is ignored. The last matching rule wins, so a later `!pattern` can re-include.
func (m *Matcher) Match(relPath string) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	segs := strings.Split(relPath, "/")
	ignored := false
	for _, r := range m.rules {
		if r.match(segs) {
			ignored = !r.neg
		}
	}
	return ignored
}

// PruneDir reports whether nothing under relDir (a forward-slash, sync-root-
// relative directory) can be un-ignored, so a walker may skip it without
// reading its contents. Every path beneath the directory is matched by the last
// rule that covers the directory itself; only a later negation could
// re-include one of them, so any later negation keeps the directory open.
func (m *Matcher) PruneDir(relDir string) bool {
	if m == nil {
		return false
	}
	ds := strings.Split(relDir, "/")
	last := -1
	for i, r := range m.rules {
		if r.coversDir(ds) {
			last = i
		}
	}
	if last < 0 || m.rules[last].neg {
		return false
	}
	for _, r := range m.rules[last+1:] {
		if r.neg {
			return false
		}
	}
	return true
}

// coversDir reports whether the rule matches directory segments ds or one of
// their ancestors, and therefore every path beneath ds.
func (r rule) coversDir(ds []string) bool {
	if r.anchored {
		for k := 1; k <= len(ds); k++ {
			if matchSegs(r.segs, ds[:k]) {
				return true
			}
		}
		return false
	}
	for start := 0; start < len(ds); start++ {
		for end := start + 1; end <= len(ds); end++ {
			if matchSegs(r.segs, ds[start:end]) {
				return true
			}
		}
	}
	return false
}

// match reports whether the rule matches file path segments ps. A rule matches
// either the file itself or one of its ancestor directories (so a directory rule
// ignores everything beneath it).
func (r rule) match(ps []string) bool {
	if r.anchored {
		// Consume the pattern from the root: matching all of ps hits the file;
		// matching a proper prefix hits an ancestor directory.
		for k := 0; k <= len(ps); k++ {
			if r.dirOnly && k == len(ps) {
				continue // a directory rule needs something beneath it
			}
			if matchSegs(r.segs, ps[:k]) {
				return true
			}
		}
		return false
	}
	// Unanchored: the pattern may match starting at any segment.
	for start := 0; start < len(ps); start++ {
		for end := start + 1; end <= len(ps); end++ {
			if r.dirOnly && end == len(ps) {
				continue
			}
			if matchSegs(r.segs, ps[start:end]) {
				return true
			}
		}
	}
	return false
}

// matchSegs reports whether the pattern segments exactly consume the name
// segments, with `**` matching zero or more segments and every other segment
// matched by path.Match (so `*`/`?`/`[...]` stay within one segment).
func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], name[0]); !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}
