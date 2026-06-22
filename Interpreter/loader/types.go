package loader

// ImportEntry describes a single import statement resolved to an absolute path.
// Alias is "" for a bare import (import "x.hvr";).
type ImportEntry struct {
	Alias        string // "" means bare import, no namespace prefix
	ResolvedPath string // absolute, cleaned filesystem path
	RawPath      string // the path as written in source, for error messages
	Line         int    // line of the import statement, for error messages
}

// LoadResult is the output of Load(). It contains every file reachable from
// the entry file, plus a per-file table of that file's own imports.
//
// Imports are non-transitive: Imports[fileA] only lists what fileA itself
// wrote in an `import` statement. A file never sees what its imports import.
type LoadResult struct {
	EntryPath string // absolute path of the entry file

	// Sources maps an absolute file path to its raw text content.
	Sources map[string]string

	// Imports maps an absolute file path to the list of imports that file
	// declared. Each entry has already been resolved to an absolute path
	// of another key in Sources.
	Imports map[string][]ImportEntry

	// LoadOrder lists every file in the order it was first discovered,
	// entry file first. Useful for deterministic iteration.
	LoadOrder []string
}
