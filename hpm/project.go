package hpm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectPackages maps every qualified package name this project depends on
// to the directory holding its sources, for `import <@pkg/file.hvr>`.
//
// Read from the LOCKFILE, not the manifest: the manifest says what versions
// are acceptable, the lockfile says which ones were chosen. Compiling must
// use exactly what was installed, and must never trigger a resolution — a
// build that silently reaches for the network because a manifest changed is
// how you get a compile that succeeds on one machine and not another.
//
// A directory with no project is not an error. Compiling a standalone .hvr
// file that imports nothing but the standard library has to keep working
// from anywhere, so a missing hover.toml simply means no packages.
func ProjectPackages(startDir string) (map[string]string, error) {
	root, err := FindProjectRoot(startDir)
	if err != nil {
		return nil, nil // no project here — stdlib and relative imports only
	}

	lock, found, err := LoadLockfile(root)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	out := make(map[string]string, len(lock.Packages))
	var missing []string
	for _, p := range lock.Packages {
		dir, err := CacheDir(p.Hash)
		if err != nil {
			return nil, err
		}
		if !dirExists(dir) {
			missing = append(missing, p.Name)
			continue
		}
		out[p.Name] = dir
	}

	if len(missing) > 0 {
		// Reported, not fatal: only an import that actually names a missing
		// package should fail, and the loader produces a far better error
		// for that (it knows the importing file and line). A file that
		// imports none of them must still compile.
		sort.Strings(missing)
		fmt.Fprintf(os.Stderr, "[hpm] %d locked package(s) are not in the cache: %s — run `hover hpm install`\n",
			len(missing), strings.Join(missing, ", "))
	}

	return out, nil
}

// ProjectPackagesForFile is the entry point the compiler uses: it resolves
// packages for whatever project contains the given source file.
func ProjectPackagesForFile(entryFile string) (map[string]string, error) {
	abs, err := filepath.Abs(entryFile)
	if err != nil {
		return nil, err
	}
	return ProjectPackages(filepath.Dir(abs))
}
