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

// ImportEntry describes a single import statement resolved to the directory
// it names and the files inside it.
type ImportEntry struct {
	// Qualifier is the name this import binds. Every whole-directory import
	// is qualified: names from it are reached as Qualifier.Name. Defaults to
	// the last path segment, overridden by `as`.
	Qualifier string

	Alias string // the explicit `as` name, or "" when the default was used

	// ResolvedDir is the absolute directory the import names; Files are the
	// .hvr files directly inside it, sorted. All of them are loaded and all
	// of them share one namespace.
	ResolvedDir string
	Files       []string

	RawPath  string // the path as written in source, for error messages
	Line     int    // line of the import statement, for error messages
	IsSystem bool   // true for `import <...>`, false for `import "..."`

	// Selective and Selected describe the `from ... import ...` form. Which
	// directory to load is identical either way; these record which names
	// were asked for, and mean the qualifier is not used.
	Selective bool
	Selected  []SelectedSymbol

	// Package is the package name for an import resolved through the
	// lockfile, or "" for bundled-stdlib and relative imports.
	Package string
}

// LoadResult is the output of Load(). It contains every file reachable from
// the entry file, plus a per-file table of that file's own imports.
//
// Two kinds of grouping matter here and they are not the same:
//
//   - Files are still loaded, lexed and parsed INDIVIDUALLY, so every error
//     keeps an accurate file and line.
//   - Names are scoped by DIRECTORY. Sibling .hvr files share one namespace,
//     which is what makes an import name a directory rather than a file.
//
// Imports remain non-transitive: Imports[fileA] lists only what fileA itself
// wrote. A file never sees what its imports import.
type LoadResult struct {
	EntryPath string // absolute path of the entry file

	// Sources maps an absolute file path to its raw text content.
	Sources map[string]string

	// Imports maps an absolute file path to the list of imports that file
	// declared, each already resolved to a directory and its files.
	Imports map[string][]ImportEntry

	// LoadOrder lists every file in the order it was first discovered,
	// entry file first. Useful for deterministic iteration.
	LoadOrder []string
}
