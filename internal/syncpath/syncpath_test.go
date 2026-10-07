package syncpath

import (
	"errors"
	"strings"
	"testing"
)

var (
	linux   = Platform{}
	windows = Platform{Windows: true, CaseInsensitive: true}
)

func TestInvalidEverywhere(t *testing.T) {
	for _, p := range []string{
		"",
		"/etc/passwd",
		"../escape.txt",
		"a/../../escape.txt",
		"a/./b",
		"a//b",
		"a/",
		"C:/Windows/win.ini",
		"c:relative",
		"a\x00b",
		"bad\xffutf8",
		".tendrils-root",
		".tendrils-root.tmp-1",
		".tendrils-trash/a.md",
		"notes/.tendrils-tmp-123",
	} {
		for _, plat := range []Platform{linux, windows} {
			if err := Check(p, plat); !errors.Is(err, ErrInvalid) {
				t.Errorf("Check(%q, %+v) = %v, want ErrInvalid", p, plat, err)
			}
		}
	}
}

func TestBookkeepingInAnotherCaseIsReservedOnlyWhereCaseFolds(t *testing.T) {
	for _, p := range []string{".TENDRILS-ROOT", ".Tendrils-Trash/a.md", "x/.Tendrils-Tmp-9"} {
		if err := Check(p, windows); !errors.Is(err, ErrInvalid) {
			t.Errorf("windows Check(%q) = %v, want ErrInvalid", p, err)
		}
		if err := Check(p, linux); err != nil {
			t.Errorf("linux Check(%q) = %v, want nil", p, err)
		}
	}
}

func TestBlockedOnWindowsOnly(t *testing.T) {
	for _, p := range []string{
		"CON",
		"music/con.flac",
		"aux .txt",
		"COM1.log",
		"lpt9",
		"LPT¹.txt",
		"report.txt:secret",
		"a.txt::$DATA",
		"what?.md",
		`back\slash.md`,
		"trailing.",
		"trailing /x",
		"ctrl\x01char",
		"PROGRA~1/x",
		"REPORT~2.TXT",
		"dir/" + strings.Repeat("é", 200),
	} {
		err := Check(p, windows)
		if !Blocked(err) {
			t.Errorf("windows Check(%q) = %v, want blocked", p, err)
		}
	}
	for _, p := range []string{"CON", "report.txt:secret", "what?.md", `back\slash.md`, "trailing.", "PROGRA~1/x"} {
		if err := Check(p, linux); err != nil {
			t.Errorf("linux Check(%q) = %v, want nil", p, err)
		}
	}
}

func TestComponentLength(t *testing.T) {
	ok := "dir/" + strings.Repeat("a", MaxComponent)
	if err := Check(ok, linux); err != nil {
		t.Errorf("255-byte component: %v", err)
	}
	if err := Check(ok, windows); err != nil {
		t.Errorf("255-unit component on windows: %v", err)
	}
	long := "dir/" + strings.Repeat("a", MaxComponent+1)
	for _, plat := range []Platform{linux, windows} {
		if !Blocked(Check(long, plat)) {
			t.Errorf("256-byte component not blocked on %+v", plat)
		}
	}
	// 127 two-byte runes: 254 bytes and 127 UTF-16 units — valid on both.
	if err := Check(strings.Repeat("é", 127), windows); err != nil {
		t.Errorf("multibyte name within limits: %v", err)
	}
}

func TestLegitimateNamesPass(t *testing.T) {
	for _, p := range []string{
		"a.md",
		"música/日本語/ファイル.flac",
		"Artist - Title (Remix) [2024].flac",
		".tendrilsignore",
		"notes/.hidden",
		"con.d/settings",
		"COM10.txt",
		"~tilde~/file~1x.txt",
		"name.with.dots.tar.gz",
	} {
		if err := Check(p, linux); err != nil {
			t.Errorf("linux Check(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"a.md", "música/日本語/ファイル.flac", "COM10.txt", "name.with.dots.tar.gz", ".tendrilsignore"} {
		if err := Check(p, windows); err != nil {
			t.Errorf("windows Check(%q) = %v", p, err)
		}
	}
}

func TestCollisions(t *testing.T) {
	got := Collisions([]string{"README.md", "Readme.md", "notes/a.md", "Docs/a.md", "docs/b.md", "docs2/c.md"})
	for _, p := range []string{"README.md", "Readme.md", "Docs/a.md", "docs/b.md"} {
		if _, ok := got[p]; !ok {
			t.Errorf("%q not reported as colliding", p)
		}
	}
	for _, p := range []string{"notes/a.md", "docs2/c.md"} {
		if _, ok := got[p]; ok {
			t.Errorf("%q wrongly reported as colliding: %s", p, got[p])
		}
	}
	if n := len(Collisions([]string{"a/b.md", "a/c.md", "A.md"})); n != 0 {
		t.Errorf("file and directory sharing a folded stem but not a name collided: %d", n)
	}
}
