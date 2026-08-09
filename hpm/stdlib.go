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

// ─────────────────────────────────────────────────────────────────────────────
// THE MACHINE-WIDE PROJECT
//
// ~/.hover is itself a project: an ordinary hover.toml and hover.lock, read
// and written by exactly the code that handles any other. It holds the
// standard library, plus anything installed while not standing in a project
// of your own.
//
// Making it a real project rather than a special store is what removes the
// init step: `hpm install foo` outside a project resolves, locks and reports
// through the same path as inside one, and `hpm list`, `hpm remove` and
// `hpm update` work there with no new code. The stdlib is then not a special
// case at all — it is one dependency in that manifest.
// ─────────────────────────────────────────────────────────────────────────────

// machineProjectDir returns ~/.hover, creating its manifest on first use.
func machineProjectDir() (string, error) {
	home, err := HoverHome()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", fmt.Errorf("could not create %s: %w", home, err)
	}
	if !fileExists(filepath.Join(home, ManifestName)) {
		if _, err := InitManifest(home, "hover-machine"); err != nil {
			return "", err
		}
	}
	return home, nil
}

// MachineLockPath is ~/.hover/hover.lock.
func MachineLockPath() (string, error) {
	h, err := HoverHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, LockName), nil
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

// InstallStdlib makes sure the machine-wide project declares the standard
// library for this compiler version, then installs it.
//
// It resolves the WHOLE machine project, not just the stdlib: anything else
// installed there gets re-checked at the same time, which is what makes
// `hover --setup` a repair step for a broken ~/.hover rather than a
// stdlib-only one.
//
// Update is forced: `hover --setup` is an explicit user action, and the point
// of publishing the stdlib is that a patch reaches people without waiting for
// a compiler release. If the network is unavailable but what is already
// installed is complete, that is reported and kept rather than failed — a
// working install must not be broken by a transient outage.
func InstallStdlib(ctx context.Context, compilerVersion string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	dir, err := machineProjectDir()
	if err != nil {
		return err
	}
	m, err := LoadManifest(dir)
	if err != nil {
		return err
	}
	req := StdlibRequirement(compilerVersion)

	// Record the requirement in ~/.hover/hover.toml, so the standard library
	// is visible as an ordinary dependency — `hpm list` shows it, and it is
	// editable by anyone who wants a different one.
	if cur, found := m.Dep(StdlibPackageName); !found || cur.Version != req {
		m.AddIndexedDep(StdlibPackageName, req)
		if err := m.Save(); err != nil {
			return err
		}
		// Re-read, so resolution runs against the bytes on disk rather than
		// an in-memory model that could differ from them: the edit above
		// touches the TOML document, not the parsed dependency list, so
		// resolving `m` as it stands would see no stdlib dependency at all
		// and install nothing.
		if m, err = LoadManifest(dir); err != nil {
			return err
		}
	}

	lock, _, err := LoadLockfile(dir)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "[Setup] Installing the standard library (%s)...\n", req)

	packages, err := resolveMachine(ctx, m, lock, Options{Update: true, Out: out})
	if err != nil {
		// Fall back to whatever is already installed, but only if it is
		// complete — a partial stdlib that fails later at compile time is
		// worse than a setup that says what went wrong now.
		if kept, keptErr := resolveMachine(ctx, m, lock, Options{Offline: true, Out: io.Discard}); keptErr == nil && len(kept) > 0 {
			fmt.Fprintf(out, "[Setup] Could not reach the package index: %v\n", err)
			fmt.Fprintf(out, "[Setup] Keeping what is already installed (%s).\n", describeVersions(onlyStdlib(kept)))
			return nil
		}
		return fmt.Errorf("could not install the standard library: %w", err)
	}

	lock.Packages = packages
	if err := lock.Save(); err != nil {
		return err
	}

	fmt.Fprintf(out, "[Setup] Standard library ready: %s\n", describeVersions(onlyStdlib(packages)))
	if extra := len(packages) - 1; extra > 0 {
		fmt.Fprintf(out, "[Setup] %d other machine-wide package(s) checked.\n", extra)
	}
	return nil
}

func resolveMachine(ctx context.Context, m *Manifest, lock *Lockfile, opts Options) ([]LockedPackage, error) {
	// A fresh lock view per attempt: Resolve mutates nothing, but the
	// fallback pass must see the ON-DISK lock, not one a failed run touched.
	view := &Lockfile{Path: lock.Path, Packages: append([]LockedPackage(nil), lock.Packages...)}
	return NewResolver(m, view, opts).Resolve(ctx)
}

// onlyStdlib narrows a resolution result to the standard library, so
// --setup reports on what it claims to be doing. The machine project may
// hold anything the user installed there, and listing those under "standard
// library ready" would be a lie in the one message people read.
func onlyStdlib(packages []LockedPackage) []LockedPackage {
	for _, p := range packages {
		if p.Name == StdlibPackageName {
			return []LockedPackage{p}
		}
	}
	return nil
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
	return machineRoots(true)
}

// MachineRoots is every package installed machine-wide, expanded.
func MachineRoots() (map[string]string, error) {
	return machineRoots(false)
}

func machineRoots(stdlibOnly bool) (map[string]string, error) {
	lockPath, err := MachineLockPath()
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
		if stdlibOnly && p.Name != StdlibPackageName {
			continue
		}
		dir, err := CacheDir(p.Hash)
		if err != nil {
			return nil, err
		}
		if !dirExists(dir) {
			continue
		}
		if p.Name == StdlibPackageName {
			for name, d := range stdlibRootsIn(dir) {
				out[name] = d
			}
			continue
		}
		out[p.Name] = dir
	}
	return out, nil
}

// PackageRootsForFile is the compiler's single entry point for "what packages
// can this file see?", and the one place the policy lives.
//
//	inside a project:  the project's hover.lock, plus the machine's STANDARD
//	                   LIBRARY — and nothing else machine-wide
//	outside any:       everything installed machine-wide
//
// The asymmetry is the important part. A project declares its dependencies,
// so a build of it must not silently pick up whatever happens to be installed
// on this particular machine — that is precisely the failure where a project
// compiles for its author and not for anyone else, and it cannot be diagnosed
// from the project's own files. Outside a project there is nothing to be
// reproducible against and nothing to break, so the convenience is free.
//
// The standard library crosses that line because it is the language's own
// library: every file may assume it, and a project that wants a specific one
// pins `stdlib` in its manifest, which then wins here by name.
func PackageRootsForFile(entryFile string) (map[string]string, error) {
	abs, err := filepath.Abs(entryFile)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)

	inProject := true
	if _, err := FindProjectRoot(dir); err != nil {
		inProject = false
	}

	roots, err := machineRoots(inProject)
	if err != nil {
		return nil, err
	}
	if roots == nil {
		roots = map[string]string{}
	}

	if inProject {
		project, err := ProjectPackages(dir)
		if err != nil {
			return nil, err
		}
		for name, d := range project {
			roots[name] = d
		}
	}
	return ExpandStdlibRoots(roots), nil
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

// MachineCacheDirs returns the cache directory names the machine-wide
// project occupies, so `hpm clean` in some unrelated project does not delete
// them. Keyed by base name, matching how clean enumerates the cache.
func MachineCacheDirs() map[string]bool {
	keep := map[string]bool{}
	lockPath, err := MachineLockPath()
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
