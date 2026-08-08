package hpm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE STANDARD LIBRARY AS PACKAGES
//
// The stdlib is not special-cased anywhere in the compiler: `import <math>`
// resolves through exactly the same package table as `import <hvr-rc>`. What
// makes it feel built in is only that `hover --setup` installs it for you,
// into a machine-wide lock rather than a project one.
//
// It ships as ONE package — one archive, one hash, one index entry — whose
// root holds the four top-level directories. That is a packaging decision,
// not a language one: the compiler still resolves `import <math>` through the
// ordinary package table, because the installed tree is EXPANDED into one
// package root per top-level directory (see stdlibRootsIn).
//
// The alternative was four packages, which the first segment of an angle
// import makes the more literal reading — `import <math>` naming a package
// called "math". One package wins on the things that actually matter here:
// the four are versioned, released and tested together, so four separate
// versions could only ever be in lockstep or wrong; and a single archive
// means --setup makes one request instead of four, on the one code path
// every new install must survive.
//
// The cost is that `math` cannot be upgraded without upgrading the rest.
// Given they are one release artifact built from one source tree, that is
// not a real loss.
// ─────────────────────────────────────────────────────────────────────────────

// StdlibPackageName is the single package `hover --setup` installs.
const StdlibPackageName = "stdlib"

// StdlibPackages are the top-level directories inside that package, each of
// which becomes an importable package root. Used for diagnostics, and as the
// expected contents when validating an install.
var StdlibPackages = []string{
	"electromechanical",
	"math",
	"optoelectronics",
	"semiconductors",
}

// StdlibLockName is the machine-wide lock recording which stdlib is
// installed. Deliberately NOT called hover.lock: $HOVER_HOME can point at a
// directory that is also a project, and two files with the same name meaning
// different things is how a project lock ends up silently governing the
// standard library.
const StdlibLockName = "stdlib.lock"

// StdlibLockPath is ~/.hover/stdlib.lock.
func StdlibLockPath() (string, error) {
	h, err := HoverHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, StdlibLockName), nil
}

// StdlibRequirement turns a compiler version into the requirement used to
// resolve the standard library: hover 0.8.3 asks for "^0.8.0".
//
// The stdlib version tracks the compiler's minor release rather than moving
// independently, so "which stdlib does hover 0.8 give me?" has an answer.
// The caret then lets a stdlib patch reach users without a compiler release,
// which is the whole reason for shipping it this way.
func StdlibRequirement(compilerVersion string) string {
	v := strings.TrimPrefix(strings.TrimSpace(compilerVersion), "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "*"
	}
	return fmt.Sprintf("^%s.%s.0", parts[0], parts[1])
}

// stdlibManifest builds the synthetic project the resolver walks. There is no
// hover.toml on disk for it — the standard library's dependency set is a
// property of the compiler build, not something a user edits.
func stdlibManifest(compilerVersion string) (*Manifest, error) {
	home, err := HoverHome()
	if err != nil {
		return nil, err
	}
	req := StdlibRequirement(compilerVersion)

	return &Manifest{
		Dir:     home,
		Path:    "<standard library>",
		Name:    "hover-stdlib",
		Version: compilerVersion,
		Deps:    []Dependency{{Name: StdlibPackageName, Version: req}},
	}, nil
}

// InstallStdlib resolves and installs the standard library for this compiler
// version, writing ~/.hover/stdlib.lock.
//
// Update is forced: `hover --setup` is an explicit user action, and the point
// of publishing the stdlib is that a patch reaches people without waiting for
// a compiler release. If the network is unavailable but the existing lock is
// complete and cached, that is reported and kept rather than failed — a
// working install must not be broken by a transient outage.
func InstallStdlib(ctx context.Context, compilerVersion string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	m, err := stdlibManifest(compilerVersion)
	if err != nil {
		return err
	}
	lockPath, err := StdlibLockPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("could not create %s: %w", filepath.Dir(lockPath), err)
	}

	lock, _, err := LoadLockfileAt(lockPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "[Setup] Installing the standard library (%s)...\n", StdlibRequirement(compilerVersion))

	packages, err := resolveStdlib(ctx, m, lock, Options{Update: true, Out: out})
	if err != nil {
		// Fall back to whatever is already installed, but only if it is
		// complete — a partial stdlib that fails later at compile time is
		// worse than a setup that says what went wrong now.
		if kept, keptErr := resolveStdlib(ctx, m, lock, Options{Offline: true, Out: io.Discard}); keptErr == nil && len(kept) > 0 {
			fmt.Fprintf(out, "[Setup] Could not reach the package index: %v\n", err)
			fmt.Fprintf(out, "[Setup] Keeping the standard library already installed (%s).\n", describeVersions(kept))
			return nil
		}
		return fmt.Errorf("could not install the standard library: %w", err)
	}

	lock.Path = lockPath
	lock.Packages = packages
	if err := lock.Save(); err != nil {
		return err
	}

	fmt.Fprintf(out, "[Setup] Standard library ready: %s\n", describeVersions(packages))
	return nil
}

func resolveStdlib(ctx context.Context, m *Manifest, lock *Lockfile, opts Options) ([]LockedPackage, error) {
	// A fresh lock view per attempt: Resolve mutates nothing, but the
	// fallback pass must see the ON-DISK lock, not one a failed run touched.
	view := &Lockfile{Path: lock.Path, Packages: append([]LockedPackage(nil), lock.Packages...)}
	return NewResolver(m, view, opts).Resolve(ctx)
}

func describeVersions(packages []LockedPackage) string {
	var parts []string
	for _, p := range packages {
		if p.Version != "" {
			parts = append(parts, p.Name+" "+p.Version)
		} else {
			parts = append(parts, p.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// StdlibRoots maps each installed standard-library package to its directory
// in the content-addressed cache.
//
// Read from the lock, never resolved: compiling must not touch the network,
// exactly as for project packages. A missing lock is not an error here — the
// caller turns "no stdlib" into a good message only when an import actually
// needs one, so a file importing nothing still compiles on a machine that has
// never run --setup.
func StdlibRoots() (map[string]string, error) {
	lockPath, err := StdlibLockPath()
	if err != nil {
		return nil, err
	}
	lock, found, err := LoadLockfileAt(lockPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	out := map[string]string{}
	for _, p := range lock.Packages {
		dir, err := CacheDir(p.Hash)
		if err != nil {
			return nil, err
		}
		if !dirExists(dir) {
			continue
		}
		for name, d := range stdlibRootsIn(dir) {
			out[name] = d
		}
	}
	return out, nil
}

// stdlibRootsIn expands one installed standard-library tree into the package
// roots the compiler resolves against.
//
// This is the whole of the "one package, four import roots" trick: the
// archive holds math/, semiconductors/, optoelectronics/ and
// electromechanical/, and each becomes a package root under its own name. So
// `import <math>` resolves to <cache>/math with no knowledge anywhere in the
// compiler that math arrived inside something called stdlib.
//
// Subdirectories are read from disk rather than taken from StdlibPackages, so
// a stdlib release that adds a top-level directory works against an older
// compiler — the package table is built from what was actually installed.
//
// "stdlib" itself is also bound, which makes `import <stdlib/math>` an
// equally valid spelling of `import <math>`. Both name the same directory, so
// they cannot disagree.
func stdlibRootsIn(dir string) map[string]string {
	out := map[string]string{StdlibPackageName: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out[e.Name()] = filepath.Join(dir, e.Name())
	}
	return out
}

// ExpandStdlibRoots binds a project's own standard-library pin the same way
// --setup's does.
//
// Without this, a project that pins `stdlib = "0.8.1"` in its hover.toml
// would get a package root called "stdlib" and nothing else, and every
// `import <math>` in it would fall back to the machine-wide copy — silently
// ignoring the pin. Pinning the standard library per project has to work, or
// it is not really a package.
func ExpandStdlibRoots(roots map[string]string) map[string]string {
	dir, ok := roots[StdlibPackageName]
	if !ok {
		return roots
	}
	for name, d := range stdlibRootsIn(dir) {
		roots[name] = d
	}
	return roots
}

// StdlibCacheDirs returns the cache directory names the standard library
// occupies, so `hpm clean` does not delete it. Keyed by base name, matching
// how clean enumerates the cache.
func StdlibCacheDirs() map[string]bool {
	keep := map[string]bool{}
	lockPath, err := StdlibLockPath()
	if err != nil {
		return keep
	}
	lock, found, err := LoadLockfileAt(lockPath)
	if err != nil || !found {
		return keep
	}
	for _, p := range lock.Packages {
		if dir, err := CacheDir(p.Hash); err == nil {
			keep[filepath.Base(dir)] = true
		}
	}
	return keep
}

// StdlibImportNames are the names an import may use to reach the standard
// library: the four directories, plus "stdlib" itself. A failed import of one
// of these should say `hover --setup`, not `hover hpm install`.
func StdlibImportNames() []string {
	return append([]string{StdlibPackageName}, StdlibPackages...)
}
