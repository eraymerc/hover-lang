// Package hpm implements the Hover package manager: manifest, lockfile,
// index and cache handling behind `hover hpm ...` (and the `hpm` symlink).
//
// The design and the reasoning behind it live in docs/package-manager-design.md.
// The two properties everything else follows from:
//
//   - There is no registry. hover-lang.org publishes a static git repo of
//     pointers; package bytes always come from upstream. Nothing here talks
//     to a hover-lang.org service, because there isn't one.
//   - Every fetched package is content-addressed. The lockfile records the
//     hash, the cache is keyed by it, and a mismatch is a hard error rather
//     than a warning — that is what turns "upstream moved a tag under me"
//     from wrong code into a clean failure.
package hpm

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// ManifestName is the per-project dependency declaration. Declarative
	// TOML, never executable — see the design doc on why setup.py's model
	// was rejected.
	ManifestName = "hover.toml"

	// LockName is generated, and meant to be committed.
	LockName = "hover.lock"

	// OfficialIndex is the name reserved for the index shipped by the
	// project. Unqualified package names resolve against it and nothing
	// else, so an added index can never shadow an official name.
	OfficialIndex = "official"
)

// DefaultOfficialIndexURL is where the official index is cloned from.
// Overridable through HOVER_INDEX_URL, which exists for testing and for
// air-gapped mirrors — not as a supported way to replace the official index
// with a different one (add a named index for that, so it shows up in a
// diff and its packages stay qualified).
const DefaultOfficialIndexURL = "https://hover-lang.org/packages/index.tar.gz"

// officialIndexURL resolves the official index location for this process.
func officialIndexURL() string {
	if u := os.Getenv("HOVER_INDEX_URL"); u != "" {
		return u
	}
	return DefaultOfficialIndexURL
}

// HoverHome is the user-scoped root for everything hpm stores: ~/.hover,
// or $HOVER_HOME if set.
//
// Deliberately user-scoped rather than executable-relative, unlike
// stdlib. Dependencies must not be written into a possibly
// root-owned install directory — a `hover hpm install` run by a normal user
// against a /usr/local install would otherwise fail on permissions, or worse,
// succeed once under sudo and be unreadable afterwards.
func HoverHome() (string, error) {
	if h := os.Getenv("HOVER_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not locate your home directory: %w", err)
	}
	return filepath.Join(home, ".hover"), nil
}

// IndexRoot is ~/.hover/index — one subdirectory per configured index, each
// a plain git clone the user owns. This is the pacman property worth
// copying: once cloned, resolution never needs the network again, so an
// index outage degrades discovery of NEW packages and nothing else.
func IndexRoot() (string, error) {
	h, err := HoverHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "index"), nil
}

// IndexDir is the clone directory for one named index.
func IndexDir(name string) (string, error) {
	root, err := IndexRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// CacheRoot is ~/.hover/hpm — the content-addressed package store, shared
// across every project on the machine. Two projects pinning the same
// version of the same package share one directory, because the key is the
// content hash and identical content hashes identically.
func CacheRoot() (string, error) {
	h, err := HoverHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "hpm"), nil
}

// CacheDir is where a package with the given content hash lives.
func CacheDir(hash string) (string, error) {
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, sanitizeHash(hash)), nil
}

// sanitizeHash turns "sha256:abcd..." into a directory-safe "sha256-abcd...".
// The colon is legal on Unix but not on Windows, and the cache has to be
// laid out identically on both or a lockfile stops being portable.
func sanitizeHash(hash string) string {
	out := make([]byte, 0, len(hash))
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// FindProjectRoot walks up from startDir looking for a hover.toml, and
// returns the directory containing it.
//
// Walking up rather than requiring cwd to be the project root is what makes
// `hover hpm install` work from anywhere inside a project, the way git and
// cargo do. It stops at the filesystem root and, defensively, at the user's
// home directory: a stray hover.toml in $HOME should not silently claim
// every project underneath it.
func FindProjectRoot(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()

	for {
		if fileExists(filepath.Join(dir, ManifestName)) {
			return dir, nil
		}
		if home != "" && dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no %s found in %s or any parent directory — run `hover hpm init` to create one",
		ManifestName, startDir)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
