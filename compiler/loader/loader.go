package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load starts at entryPath and recursively discovers every .hvr file
// reachable via import statements. Each import path is resolved relative
// to the directory of the file that declared it (not the entry file, and
// not the current working directory).
//
// Imports are non-transitive: this function still needs to load every file
// in the chain (a file's content must be read to elaborate it), but the
// per-file Imports table in the result only records what each file itself
// imports — callers (the elaborator) are responsible for not leaking
// transitive visibility.
//
// File-level cycles (a.hvr imports b.hvr imports a.hvr) are detected and
// reported as an error, since a file importing itself transitively is
// always a mistake — there is no valid non-transitive interpretation of it.
func Load(entryPath string) (*LoadResult, error) {
	absEntry, err := filepath.Abs(entryPath)
	if err != nil {
		return nil, fmt.Errorf("could not resolve entry path %q: %w", entryPath, err)
	}
	absEntry = filepath.Clean(absEntry)

	result := &LoadResult{
		EntryPath: absEntry,
		Sources:   make(map[string]string),
		Imports:   make(map[string][]ImportEntry),
		LoadOrder: []string{},
	}

	visiting := make(map[string]bool) // currently on the DFS stack — for cycle detection
	visited := make(map[string]bool)  // fully processed — avoids re-reading shared files

	if err := loadFile(absEntry, result, visiting, visited); err != nil {
		return nil, err
	}

	return result, nil
}

// loadFile reads filePath, scans it for import statements, resolves each
// import relative to filePath's directory, and recurses into each import
// that hasn't been fully loaded yet.
func loadFile(filePath string, result *LoadResult, visiting map[string]bool, visited map[string]bool) error {
	if visiting[filePath] {
		return fmt.Errorf("import cycle detected: %s", describeCycle(filePath, visiting))
	}
	if visited[filePath] {
		return nil // already loaded via another import path — fine, just skip
	}

	visiting[filePath] = true
	defer delete(visiting, filePath)

	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("could not read imported file %q: %w", filePath, err)
	}
	content := string(contentBytes)

	result.Sources[filePath] = content
	result.LoadOrder = append(result.LoadOrder, filePath)

	fileDir := filepath.Dir(filePath)
	scanned := scanSourceForImports(content)

	entries := make([]ImportEntry, 0, len(scanned))
	for _, imp := range scanned {
		resolved := resolveImportPath(fileDir, imp.PathLiteral, imp.IsSystem)
		entries = append(entries, ImportEntry{
			Alias:        imp.Alias,
			ResolvedPath: resolved,
			RawPath:      imp.PathLiteral,
			Line:         imp.Line,
			IsSystem:     imp.IsSystem,
		})

		if err := loadFile(resolved, result, visiting, visited); err != nil {
			return fmt.Errorf("in %s, line %d: %w", filePath, imp.Line, err)
		}
	}

	result.Imports[filePath] = entries
	visited[filePath] = true
	return nil
}

// describeCycle builds a human-readable chain like "a.hvr -> b.hvr -> a.hvr"
// from the set of files currently on the DFS stack, for a clearer error
// message than just naming the file that closed the loop.
func describeCycle(closingFile string, visiting map[string]bool) string {
	names := make([]string, 0, len(visiting)+1)
	for f := range visiting {
		names = append(names, filepath.Base(f))
	}
	names = append(names, filepath.Base(closingFile))
	return strings.Join(names, " -> ")
}
