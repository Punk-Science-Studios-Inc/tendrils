package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.1", "0.4.0", -1},
		{"0.4.0", "0.3.1", 1},
		{"0.4.0", "0.4.0", 0},
		{"v0.4.0", "0.4.0", 0}, // the tag form and the reported form are one version
		{"1.0.0", "0.9.9", 1},
		{"0.10.0", "0.9.0", 1}, // not a string comparison
		{"0.4.1", "0.4.0", 1},
		{"1.0.0-rc.1", "1.0.0", -1}, // a prerelease precedes its release
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1}, // numeric identifiers compare numerically
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-rc.1", "1.0.0-rc.1.1", -1},
		{"1.0.0+build.5", "1.0.0+build.9", 0}, // build metadata is not precedence
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Errorf("CompareVersions(%q, %q): %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// "dev" is what buildinfo reports for every source build, and a malformed
// version must be an error rather than something that sorts somewhere. An
// updater that guessed here would offer a downgrade to a device it could not
// place.
func TestMalformedVersionsAreRejected(t *testing.T) {
	for _, v := range []string{"", "dev", "0.4", "0.4.0.1", "x.y.z", "0.4.0-", "v", "01.2.3", "latest"} {
		if _, err := CompareVersions(v, "1.0.0"); err == nil {
			t.Errorf("%q should not parse as a version", v)
		}
		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true", v)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	for _, v := range []string{"1.0.0-rc.1", "v0.1.0-beta", "0.1.0-next.3"} {
		if !IsPrerelease(v) {
			t.Errorf("IsPrerelease(%q) = false", v)
		}
	}
	// A source build is not a prerelease: it must not be offered release
	// candidates on the strength of being unversioned.
	for _, v := range []string{"1.0.0", "dev", "", "0.4.0+dirty"} {
		if IsPrerelease(v) {
			t.Errorf("IsPrerelease(%q) = true", v)
		}
	}
}
