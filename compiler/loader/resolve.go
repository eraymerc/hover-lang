package loader

import (
	"os"
	"path/filepath"
	"strings"
)

// ExeDir returns the directory containing the running hover executable
// (symlinks resolved) — the anchor for locating every resource shipped
// alongside it (standard_library, runtime lib/headers, bundled toolchain),
// independent of the caller's current working directory or the source
// file's location. This is what makes hover usable as `hover foo.hvr` from
// any directory once it's on PATH.
func ExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, e := filepath.EvalSymlinks(exe); e == nil {
		exe = real
	}
	return filepath.Dir(exe), nil
}

// stdlibRoot is the standard library directory, always a "standard_library"
// folder next to the hover executable — not the current working directory,
// and not the source file's directory. This is what makes `import <...>`
// location-independent.
func stdlibRoot() (string, error) {
	dir, err := ExeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "standard_library"), nil
}

// PackageRoots maps a qualified package name to the directory its sources
// were unpacked into. It is set once per compilation by Load(), from the
// project's lockfile, and read by ResolveImportPath.
//
// A package-level variable rather than a parameter threaded through every
// call site because path resolution happens in two places — here, and again
// in the elaborator when it re-derives which loaded file an import statement
// refers to — and those two MUST agree exactly or an import silently
// resolves to a file that was never loaded. One compilation, one project,
// one map; a caller that wants a different project calls Load again.
var PackageRoots map[string]string

// SetPackageRoots installs the package→directory table for this compilation.
// Called by Load; exported so a host embedding the compiler can supply its
// own resolution without going through a lockfile on disk.
func SetPackageRoots(roots map[string]string) { PackageRoots = roots }

// ResolveImportPath turns an import into an absolute file path.
//
//	import <a/b.hvr>       ->  {binaryDir}/standard_library/a/b.hvr
//	import <@pkg/b.hvr>    ->  {package cache dir for "pkg"}/b.hvr
//	import <@idx:pkg/b.hvr>->  {package cache dir for "idx:pkg"}/b.hvr
//	import "./a/b.hvr"     ->  {importerDir}/a/b.hvr
//
// stdlibRoot: the standard_library dir shipped next to the hover binary
// (Makefile copies it there), independent of cwd or source location.
//
// An unresolvable package returns a path under a sentinel directory rather
// than an error, so the "could not read imported file" diagnostic — which
// knows the importing file and line — is what the user sees, instead of a
// second, worse error path here that does not.
func ResolveImportPath(currentFileDir, importPath string, isSystem bool) string {
	if isSystem {
		if pkg, rest, ok := splitPackagePath(importPath); ok {
			root, found := PackageRoots[pkg]
			if !found {
				root = filepath.Join(missingPackageRoot, filepath.FromSlash(pkg))
			}
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(rest)))
		}
		if root, err := stdlibRoot(); err == nil {
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(strings.TrimLeft(importPath, "/"))))
		}
		// fall through to relative if the binary can't be located
	}
	return filepath.Clean(filepath.Join(currentFileDir, importPath))
}

// missingPackageRoot is the stand-in directory for a package that is named
// in source but absent from the lockfile. It exists only so the resulting
// path is obviously wrong in the error message a few frames later.
const missingPackageRoot = "<package-not-installed>"

// splitPackagePath recognises the package form of a system import and splits
// it into the qualified package name and the path within that package:
//
//	"@foo/bar.hvr"       -> "foo",       "bar.hvr"
//	"@idx:foo/bar.hvr"   -> "idx:foo",   "bar.hvr"
//	"@foo/a/b.hvr"       -> "foo",       "a/b.hvr"
//
// The index qualifier travels with the package name because that pair is
// what identifies a package: an added index may legitimately publish a name
// the official index also uses, and they are different packages.
func splitPackagePath(importPath string) (pkg, rest string, ok bool) {
	s := strings.TrimLeft(importPath, "/")
	if !strings.HasPrefix(s, "@") {
		return "", "", false
	}
	s = s[1:]
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || slash == len(s)-1 {
		return "", "", false // "@foo" alone names no file
	}
	return s[:slash], s[slash+1:], true
}

// PackageOf returns the qualified package name a system import refers to, or
// "" if it is a plain standard-library import. Used to report which packages
// a source tree actually depends on.
func PackageOf(importPath string, isSystem bool) string {
	if !isSystem {
		return ""
	}
	pkg, _, ok := splitPackagePath(importPath)
	if !ok {
		return ""
	}
	return pkg
}

func extractAnglePath(s string) (literal, rest string, ok bool) {
	start := strings.IndexByte(s, '<')
	if start == -1 {
		return "", "", false
	}
	end := strings.IndexByte(s[start+1:], '>')
	if end == -1 {
		return "", "", false
	}
	end = start + 1 + end
	return strings.TrimSpace(s[start+1 : end]), s[end+1:], true
}

// scannedImport is a raw import found by scanSourceForImports, before any
// path resolution has happened.
type scannedImport struct {
	PathLiteral string // the string literal contents, e.g. "./bar/ex2.hvr"
	Alias       string // "" if no "as Name" clause
	Line        int    // 1-based line number where the import appears
	IsSystem    bool   // true for `import <...>` (standard library), false for `import "..."`

	// Selective and Names describe `from <path> import a, b as c;`. The
	// loader does not act on the names — which file to read is the same
	// either way — but it carries them so callers that DO care (the
	// entry-file semantic pass) don't have to re-scan the source.
	Selective bool
	Names     []SelectedSymbol
}

// scanSourceForImports performs a minimal lexical scan for `import "...";`
// and `import "..." as Name;` statements. It deliberately does NOT use the
// full lexer/parser — the loader runs before any per-file AST exists, and
// import statements need to be found before we know if the rest of the file
// is even syntactically valid yet.
//
// Only top-level, line-oriented matching is needed: Hover requires import
// statements to start with the literal keyword "import" (after optional
// leading whitespace) and to be terminated by a semicolon on the same
// logical statement. This scanner does not need to handle imports split
// across multiple lines for the path itself, since paths are single string
// literals.
func scanSourceForImports(source string) []scannedImport {
	var found []scannedImport

	lines := strings.Split(source, "\n")
	for i, rawLine := range lines {
		line := strings.TrimSpace(rawLine)

		selective := false
		var rest string
		switch {
		case hasKeywordPrefix(line, "import"):
			rest = line[len("import"):]
		case hasKeywordPrefix(line, "from"):
			// `from` is contextual in the grammar (see parser/imports.go), so
			// it only counts here if a path really follows — a statement like
			// `from = to;` must not be mistaken for an import.
			rest = line[len("from"):]
			selective = true
		default:
			continue
		}

		trimmed := strings.TrimSpace(rest)
		var pathLit, afterPath string
		var isSystem, ok bool
		if strings.HasPrefix(trimmed, "<") {
			pathLit, afterPath, ok = extractAnglePath(trimmed)
			isSystem = true
		} else {
			pathLit, afterPath, ok = extractStringLiteral(trimmed)
		}
		if !ok {
			continue
		}

		afterPath = strings.TrimSpace(afterPath)

		if selective {
			if !hasKeywordPrefix(afterPath, "import") {
				continue // not an import statement after all
			}
			found = append(found, scannedImport{
				PathLiteral: pathLit,
				Line:        i + 1,
				IsSystem:    isSystem,
				Selective:   true,
				Names:       parseSelectedNames(afterPath[len("import"):]),
			})
			continue
		}

		alias := ""
		if hasKeywordPrefix(afterPath, "as") {
			alias = extractIdentifier(strings.TrimSpace(afterPath[len("as"):]))
		}

		found = append(found, scannedImport{
			PathLiteral: pathLit,
			Alias:       alias,
			Line:        i + 1,
			IsSystem:    isSystem,
		})
	}

	return found
}

// hasKeywordPrefix reports whether s starts with kw as a WHOLE WORD — so
// "importantThing" is not an `import`, and "fromage" is not a `from`. A bare
// `kw` with nothing after it also fails, since every import form needs a path.
func hasKeywordPrefix(s, kw string) bool {
	if !strings.HasPrefix(s, kw) || len(s) == len(kw) {
		return false
	}
	c := s[len(kw)]
	return isImportSeparator(c) || c == '"' || c == '<'
}

// parseSelectedNames reads the `a, b as c` list of a selective import. It is
// deliberately as forgiving as the rest of this scanner: the real parser runs
// afterwards and reports syntax errors with proper positions, so anything
// malformed here just yields fewer names rather than a failed load.
func parseSelectedNames(s string) []SelectedSymbol {
	var out []SelectedSymbol
	s = strings.TrimSuffix(strings.TrimSpace(s), ";")
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := extractIdentifier(part)
		if name == "" {
			continue
		}
		sym := SelectedSymbol{Name: name}
		after := strings.TrimSpace(part[len(name):])
		if hasKeywordPrefix(after, "as") {
			sym.Alias = extractIdentifier(strings.TrimSpace(after[len("as"):]))
		}
		out = append(out, sym)
	}
	return out
}

func isImportSeparator(b byte) bool {
	return b == ' ' || b == '\t'
}

// extractStringLiteral finds the first "..." literal in s and returns its
// contents plus everything after the closing quote.
func extractStringLiteral(s string) (literal string, rest string, ok bool) {
	start := strings.IndexByte(s, '"')
	if start == -1 {
		return "", "", false
	}
	end := strings.IndexByte(s[start+1:], '"')
	if end == -1 {
		return "", "", false
	}
	end = start + 1 + end
	return s[start+1 : end], s[end+1:], true
}

// extractIdentifier reads a leading identifier (letters, digits, underscore)
// from s, stopping at the first non-identifier character (such as ';').
func extractIdentifier(s string) string {
	end := 0
	for end < len(s) {
		c := s[end]
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !(end > 0 && isDigit) {
			break
		}
		end++
	}
	return s[:end]
}
