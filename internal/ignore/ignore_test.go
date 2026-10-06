package ignore

import (
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	lines := strings.Split(`
# per-device Obsidian UI state
.obsidian/workspace*.json
# per-device Claude settings, at any depth
settings.local.json
# Obsidian trash directory and everything under it
.trash/
# a deep-glob rule
**/cache/*.tmp
# anchored file
/secret.txt
# re-include one otherwise-ignored file
!.trash/keep.md
`, "\n")
	m := Compile(lines)

	cases := map[string]bool{
		// anchored glob, root-level .obsidian only
		".obsidian/workspace.json":                true,
		".obsidian/workspace (conflicted 1).json": true,
		".obsidian/app.json":                      false,
		"notes/.obsidian/workspace.json":          false, // anchored to root
		// unanchored basename at any depth
		".claude/settings.local.json":                  true,
		"AI Gamedev Daily/.claude/settings.local.json": true,
		".claude/commands/daily.md":                    false,
		// directory rule ignores everything beneath
		".trash/old.md":     true,
		"sub/.trash/old.md": true,
		// negation re-includes a specific file (last match wins)
		".trash/keep.md": false,
		// deep glob
		"a/b/cache/x.tmp": true,
		"cache/x.tmp":     true,
		"a/cache/x.md":    false,
		// anchored file at root only
		"secret.txt":     true,
		"sub/secret.txt": false,
		// plain unrelated file
		"notes/a.md": false,
	}
	for p, want := range cases {
		if got := m.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestEmptyAndComments(t *testing.T) {
	m := Compile([]string{"", "   ", "# just a comment", "\t# indented comment"})
	if m.Match("anything.md") {
		t.Error("empty/comment-only ignore file should match nothing")
	}
	var nilM *Matcher
	if nilM.Match("x") {
		t.Error("nil matcher should match nothing")
	}
}

func TestPruneDir(t *testing.T) {
	m := Compile([]string{"music/", "build/", "!build/keep.txt", "*.tmp", "logs/", "!logs/"})
	cases := map[string]bool{
		"music":         false, // a later negation exists, so nothing is safe to prune
		"notes":         false,
		"build":         false,
		"music/albums":  false,
		"logs":          false,
		"x.tmp":         false,
		"notes/archive": false,
	}
	for dir, want := range cases {
		if got := m.PruneDir(dir); got != want {
			t.Errorf("PruneDir(%q) = %v, want %v", dir, got, want)
		}
	}

	m = Compile([]string{"!music/keep.flac", "music/", "*.tmp"})
	cases = map[string]bool{
		"music":        true, // the earlier negation is overridden for everything beneath
		"music/albums": true,
		"notes":        false,
		"cache.tmp":    true,
		"a/cache.tmp":  true,
	}
	for dir, want := range cases {
		if got := m.PruneDir(dir); got != want {
			t.Errorf("PruneDir(%q) = %v, want %v", dir, got, want)
		}
	}

	if (*Matcher)(nil).PruneDir("music") {
		t.Error("nil matcher pruned a directory")
	}
}

// Pruning must agree with Match: a pruned directory hides only paths Match
// would also ignore.
func TestPruneDirAgreesWithMatch(t *testing.T) {
	m := Compile([]string{"music/", "!music/keep/", "music/keep/drop.wav"})
	if m.PruneDir("music") {
		t.Fatal("pruned a directory with a later re-inclusion beneath it")
	}
	if m.Match("music/keep/song.flac") {
		t.Fatal("re-included path reported ignored")
	}
	if !m.Match("music/keep/drop.wav") || !m.Match("music/other.flac") {
		t.Fatal("ignored paths reported included")
	}
}
