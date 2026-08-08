package loader

// SelectedSymbol is one name in a `from <path> import a, b as c;` list.
// Declared here rather than reused from the ast package on purpose: the
// loader runs before anything is parsed and must not depend on the AST.
type SelectedSymbol struct {
	Name  string // as declared in the imported file
	Alias string // local binding; "" means bind under Name
}

// Local returns the name this symbol is reachable as in the importing file.
func (s SelectedSymbol) Local() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Name
}

// ImportEntry describes a single import statement resolved to an absolute path.
// Alias is "" for a bare import (import "x.hvr";).
type ImportEntry struct {
	Alias        string // "" means bare import, no namespace prefix
	ResolvedPath string // absolute, cleaned filesystem path
	RawPath      string // the path as written in source, for error messages
	Line         int    // line of the import statement, for error messages
	IsSystem     bool   // true for `import <...>` (standard library), false for `import "..."`

	// Selective and Selected describe the `from ... import ...` form. Which
	// file to load is identical either way; these only record which names
	// that file was asked for, for callers doing visibility work.
	Selective bool
	Selected  []SelectedSymbol

	// Package is the qualified package name for `import <@pkg/...>`, or ""
	// for a stdlib or relative import.
	Package string
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
