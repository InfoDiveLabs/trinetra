// Package update implements Trinetra's signed-release format and the
// verification every host runs before installing a build. It is part of the
// core daemon and must import only the standard library.
package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version without build metadata.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseVersion parses "X.Y.Z" or "X.Y.Z-pre", with an optional leading "v".
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(s, "v")
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre && pre == "" {
		return Version{}, fmt.Errorf("update: bad version %q", s)
	}
	if strings.Contains(s, "+") {
		return Version{}, fmt.Errorf("update: bad version %q (build metadata not allowed)", s)
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("update: bad version %q", s)
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("update: bad version %q", s)
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return Version{}, fmt.Errorf("update: bad version %q", s)
		}
		n[i] = v
	}
	if hasPre {
		for _, id := range strings.Split(pre, ".") {
			if id == "" {
				return Version{}, fmt.Errorf("update: bad version %q", s)
			}
		}
	}
	return Version{Major: n[0], Minor: n[1], Patch: n[2], Pre: pre}, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// CompareVersions returns -1, 0 or 1 using semver precedence: a pre-release sorts before
// its final release; pre-release identifiers compare numerically when both are numeric.
func CompareVersions(a, b Version) int {
	for _, d := range [3][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	as, bs := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aErr := strconv.Atoi(as[i])
		bi, bErr := strconv.Atoi(bs[i])
		switch {
		case aErr == nil && bErr == nil:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case aErr == nil:
			return -1 // numeric identifiers sort before alphanumeric
		case bErr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}
