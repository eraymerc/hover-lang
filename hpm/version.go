package hpm

import (
	"fmt"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// VERSIONS
//
// Semver-shaped, with a deliberately small requirement language:
//
//	"1.2.0"     exactly 1.2.0
//	"^1.2.0"    >=1.2.0, <2.0.0   (compatible updates)
//	"~1.2.0"    >=1.2.0, <1.3.0   (patch updates only)
//	">=1.2.0"   anything at or above
//	"*"         anything
//
// No unions, no ranges, no pre-release ordering rules. This is the subset
// that covers what people actually write, and every operator here has one
// obvious meaning — which matters more than expressiveness for a file that
// decides what code runs on someone's machine. Anything outside the subset
// is rejected with a message naming what IS supported, rather than being
// silently reinterpreted as something close.
// ─────────────────────────────────────────────────────────────────────────────

// Version is a parsed major.minor.patch, with an optional pre-release tag.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // "" for a normal release
	Raw                 string
}

// ParseVersion accepts "1.2.3", "1.2", "1", and a leading "v", each with an
// optional "-pre" suffix. Missing components are zero, so "1.2" is 1.2.0 —
// the reading everyone expects.
func ParseVersion(s string) (Version, error) {
	raw := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")

	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("%q is not a version (expected major.minor.patch)", raw)
	}
	v := Version{Pre: pre, Raw: raw}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return Version{}, fmt.Errorf("%q is not a version (empty component)", raw)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%q is not a version (%q is not a number)", raw, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// compareVersions orders two version strings. An unparseable version sorts
// below a parseable one rather than causing an error: an index entry with a
// malformed version should be ignored during selection, not break every
// lookup of that package.
func compareVersions(a, b string) int {
	va, errA := ParseVersion(a)
	vb, errB := ParseVersion(b)
	switch {
	case errA != nil && errB != nil:
		return strings.Compare(a, b)
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	if c := cmpInt(va.Major, vb.Major); c != 0 {
		return c
	}
	if c := cmpInt(va.Minor, vb.Minor); c != 0 {
		return c
	}
	if c := cmpInt(va.Patch, vb.Patch); c != 0 {
		return c
	}
	// A release outranks its own pre-releases (1.0.0 > 1.0.0-rc1), which is
	// the one pre-release rule worth implementing; ordering rc1 against
	// beta2 is left to string comparison.
	switch {
	case va.Pre == "" && vb.Pre != "":
		return 1
	case va.Pre != "" && vb.Pre == "":
		return -1
	}
	return strings.Compare(va.Pre, vb.Pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// satisfies reports whether version meets requirement.
func satisfies(version, req string) bool {
	ok, err := checkRequirement(version, req)
	return err == nil && ok
}

// ValidateRequirement rejects a requirement string this subset cannot
// express, so a typo surfaces when the manifest is read rather than as
// "no version satisfies ..." much later.
func ValidateRequirement(req string) error {
	_, err := checkRequirement("0.0.0", req)
	return err
}

func checkRequirement(version, req string) (bool, error) {
	req = strings.TrimSpace(req)
	if req == "" || req == "*" || strings.EqualFold(req, "latest") {
		return true, nil
	}

	v, err := ParseVersion(version)
	if err != nil {
		return false, nil // an unparseable published version satisfies nothing
	}

	switch {
	case strings.HasPrefix(req, "^"):
		base, err := ParseVersion(req[1:])
		if err != nil {
			return false, err
		}
		if compareVersion(v, base) < 0 {
			return false, nil
		}
		// Caret is "compatible with", and below 1.0.0 semver says the minor
		// position carries breaking changes — so ^0.2.1 allows 0.2.x and
		// not 0.3.0. Getting this wrong is how a caret range silently
		// installs a breaking release of an early-stage package.
		if base.Major > 0 {
			return v.Major == base.Major, nil
		}
		if base.Minor > 0 {
			return v.Major == 0 && v.Minor == base.Minor, nil
		}
		return v.Major == 0 && v.Minor == 0, nil

	case strings.HasPrefix(req, "~"):
		base, err := ParseVersion(req[1:])
		if err != nil {
			return false, err
		}
		if compareVersion(v, base) < 0 {
			return false, nil
		}
		return v.Major == base.Major && v.Minor == base.Minor, nil

	case strings.HasPrefix(req, ">="):
		base, err := ParseVersion(req[2:])
		if err != nil {
			return false, err
		}
		return compareVersion(v, base) >= 0, nil

	case strings.HasPrefix(req, ">"), strings.HasPrefix(req, "<"), strings.HasPrefix(req, "="):
		if strings.HasPrefix(req, "=") && !strings.HasPrefix(req, "==") {
			base, err := ParseVersion(req[1:])
			if err != nil {
				return false, err
			}
			return compareVersion(v, base) == 0, nil
		}
		return false, fmt.Errorf("unsupported version requirement %q — hpm understands an exact version (\"1.2.0\"), \"^1.2.0\", \"~1.2.0\", \">=1.2.0\" and \"*\"", req)

	default:
		base, err := ParseVersion(req)
		if err != nil {
			return false, fmt.Errorf("unsupported version requirement %q — hpm understands an exact version (\"1.2.0\"), \"^1.2.0\", \"~1.2.0\", \">=1.2.0\" and \"*\"", req)
		}
		return compareVersion(v, base) == 0, nil
	}
}

func compareVersion(a, b Version) int { return compareVersions(a.String(), b.String()) }
