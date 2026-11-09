package loader

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// readFile and readDir are the loader's only access to source files. They
// default to the OS filesystem; a host with no disk to read from — the
// browser playground, which runs this compiler as WASM — installs an fs.FS
// instead with SetFS.
//
// Package-level for the same reason PackageRoots is: one compilation reads
// from one place, and every lookup (Load, HvrFilesIn) has to agree on it.
var (
	readFile = os.ReadFile
	readDir  = os.ReadDir
)

// SetFS makes the loader read every source file from fsys instead of the OS.
// Paths the loader computes are absolute ("/stdlib/math/math.hvr"); fs.FS
// paths are unrooted, so the leading slash is dropped on the way in.
func SetFS(fsys fs.FS) {
	rel := func(name string) string {
		p := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "/")
		if p == "" {
			return "."
		}
		return p
	}
	readFile = func(name string) ([]byte, error) { return fs.ReadFile(fsys, rel(name)) }
	readDir = func(name string) ([]os.DirEntry, error) { return fs.ReadDir(fsys, rel(name)) }
}
