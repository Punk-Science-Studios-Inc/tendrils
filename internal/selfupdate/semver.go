package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Semantic-version comparison, implemented here rather than pulled in as a
// dependency: it is forty lines, it is exactly testable, and the update path is
// the last place that should grow a supply chain.

// semver is a parsed X.Y.Z[-prerelease][+build] version. Build metadata is
// parsed and then ignored, per the spec — it takes no part in precedence.
type semver struct {
	major, minor, patch int
	pre                 string
}

// parseSemver accepts an optional leading "v", as git tags carry one and the
// release JSON does not.
func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, fmt.Errorf("selfupdate: empty version")
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i] // build metadata: parsed off, never compared
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre, s = s[i+1:], s[:i]
		if pre == "" {
			return semver{}, fmt.Errorf("selfupdate: %q has an empty prerelease", s)
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("selfupdate: %q is not a X.Y.Z version", s)
	}
	out := semver{pre: pre}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return semver{}, fmt.Errorf("selfupdate: %q is not a X.Y.Z version", s)
		}
		switch i {
		case 0:
			out.major = n
		case 1:
			out.minor = n
		case 2:
			out.patch = n
		}
	}
	return out, nil
}

// compare returns -1, 0 or 1 for a<b, a==b, a>b.
func (a semver) compare(b semver) int {
	for _, p := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if p[0] != p[1] {
			return sign(p[0] - p[1])
		}
	}
	return comparePre(a.pre, b.pre)
}

// comparePre implements semver §11.3-11.4: a version with a prerelease has
// lower precedence than the same version without one, and identifiers compare
// numerically when both are numeric, lexically otherwise.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1 // 1.0.0 > 1.0.0-rc1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aNum := strconv.Atoi(as[i])
		bn, bNum := strconv.Atoi(bs[i])
		switch {
		case aNum == nil && bNum == nil:
			if an != bn {
				return sign(an - bn)
			}
		case aNum == nil:
			return -1 // numeric identifiers rank below alphanumeric ones
		case bNum == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(as) - len(bs))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// CompareVersions compares two semantic versions, returning -1, 0 or 1. Either
// may carry a leading "v".
func CompareVersions(a, b string) (int, error) {
	av, err := parseSemver(a)
	if err != nil {
		return 0, err
	}
	bv, err := parseSemver(b)
	if err != nil {
		return 0, err
	}
	return av.compare(bv), nil
}

// IsPrerelease reports whether a version string names a prerelease. An
// unparsable version is not a prerelease — "dev" is a source build, and a
// source build must not be offered release candidates.
func IsPrerelease(v string) bool {
	sv, err := parseSemver(v)
	return err == nil && sv.pre != ""
}

// IsRelease reports whether a version string is a comparable release version.
// buildinfo reports "dev" for anything unreleased, which is precisely the case
// that cannot be compared.
func IsRelease(v string) bool {
	_, err := parseSemver(v)
	return err == nil
}
