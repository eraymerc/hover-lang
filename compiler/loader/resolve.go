package loader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExeDir returns the directory containing the running hover executable
// (symlinks resolved) — the anchor for locating every resource shipped
// alongside it (stdlib, runtime lib/headers, bundled toolchain),
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

// stdlibRoot is the standard library directory, always a "stdlib"
// folder next to the hover executable — not the current working directory,
// and not the source file's directory. This is what makes `import <...>`
// location-independent.
func stdlibRoot() (string, error) {
	dir, err := ExeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "stdlib"), nil
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

// StdlibPackageNames is the set of package names that belong to the standard
// library, used only to phrase a better error when one of them is missing.
//
// Injected by main rather than defined here, because which packages make up
// the standard library is the package manager's business, and the compiler
// importing hpm to ask would invert the layering for the sake of four
// strings. Empty is fine: the diagnostic just falls back to the generic
// "not installed" wording.
var StdlibPackageNames map[string]bool

// SetStdlibPackageNames records the standard library's package names.
func SetStdlibPackageNames(names []string) {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	StdlibPackageNames = m
}

func isStdlibPackage(name string) bool { return StdlibPackageNames[name] }

// MachineInstalledPackages reports every package installed machine-wide, and
// exists purely to phrase one diagnostic: a package that IS installed on this
// machine but is invisible here because the file sits inside a project, which
// sees only its own dependencies. Without it that case reads "package %q is
// not installed — run `hover hpm install %q`" at someone who just did exactly
// that, and the real problem — which project the file belongs to — is never
// mentioned.
//
// A function rather than a set because it is consulted only on the failure
// path: the machine lockfile should not be read on every successful compile
// for a message that is almost never printed. Injected by main for the same
// layering reason as StdlibPackageNames; nil is fine and falls back to the
// generic wording.
var MachineInstalledPackages func() []string

// SetMachineInstalledPackages installs the machine-wide package lookup.
func SetMachineInstalledPackages(fn func() []string) { MachineInstalledPackages = fn }

func isMachineInstalled(name string) bool {
	if MachineInstalledPackages == nil {
		return false
	}
	for _, n := range MachineInstalledPackages() {
		if n == name {
			return true
		}
	}
	return false
}

// SetPackageRoots installs the package→directory table for this compilation.
// Called by Load; exported so a host embedding the compiler can supply its
// own resolution without going through a lockfile on disk.
func SetPackageRoots(roots map[string]string) { PackageRoots = roots }

// ResolveImportPath turns an import into the absolute DIRECTORY it names.
//
//	import <a>          ->  package "a" if installed, else
//	                        {binaryDir}/stdlib/a
//	import <a/b>        ->  the "b" subdirectory of either
//	import <idx:a/b>    ->  package "a" from index "idx" — never stdlib
//	import "./a/b"      ->  {importerDir}/a/b
//
// An import names a directory, not a file: every `.hvr` file directly inside
// it is imported together, as one unit sharing one namespace. This is Go's
// package rule and Python's package rule, and it is why a library no longer
// has to import its own siblings.
//
// THE FIRST SEGMENT OF AN ANGLE IMPORT IS A PACKAGE NAME. There is no sigil
// distinguishing a package from the standard library, and that is the point:
// the standard library is intended to become an ordinary installable package
// itself, so `import <math>` must keep working whether math is the copy
// bundled next to the binary or a version pinned in the lockfile.
//
// Resolution order is therefore: the project's lockfile first, the bundled
// stdlib second. A project that installs a package named `math`
// overrides the bundled one — deliberately, since that is how you pin a
// standard-library version. It is an auditable decision rather than a silent
// one: everything consulted here comes from a committed hover.lock.
//
// The hazard worth knowing about is that a TRANSITIVE dependency named `math`
// also lands in that lockfile and would override it project-wide. That is
// visible in `hover.lock` and in `hover hpm list`, which is the mitigation —
// but it is a real reason to read what a new dependency drags in.
//
// An index-qualified name (`idx:a`) is only ever a package. It skips the
// stdlib fallback entirely, so an added index can never satisfy an import
// that was meant for the standard library.
func ResolveImportPath(currentFileDir, importPath string, isSystem bool) string {
	if isSystem {
		pkg, rest := splitPackagePath(importPath)
		if root, found := PackageRoots[pkg]; found {
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(rest)))
		}
		if isQualified(pkg) {
			// Qualified: a package or nothing. The sentinel makes the
			// resulting path obviously wrong in the diagnostic a few frames
			// later, which knows the importing file and line.
			return filepath.Clean(filepath.Join(missingPackageRoot,
				filepath.FromSlash(pkg), filepath.FromSlash(rest)))
		}
		if root, err := stdlibRoot(); err == nil {
			return filepath.Clean(filepath.Join(root, filepath.FromSlash(strings.TrimLeft(importPath, "/"))))
		}
		// fall through to relative if the binary can't be located
	}
	return filepath.Clean(filepath.Join(currentFileDir, filepath.FromSlash(importPath)))
}

// missingPackageRoot is the stand-in directory for an index-qualified package
// that is named in source but absent from the lockfile.
const missingPackageRoot = "<package-not-installed>"

// splitPackagePath splits a system import into its leading package name and
// the path within that package. Unlike the file-based scheme this replaced,
// a bare package name is a complete import — `<math>` names the package's
// own root directory — so rest may be empty.
//
//	"foo"          -> "foo",      ""
//	"foo/bar"      -> "foo",      "bar"
//	"idx:foo/bar"  -> "idx:foo",  "bar"
//
// An index qualifier travels with the package name because that pair is what
// identifies a package: two indexes may legitimately publish the same name,
// and they are not the same package.
func splitPackagePath(importPath string) (pkg, rest string) {
	s := strings.Trim(importPath, "/")
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return s, ""
	}
	return s[:slash], s[slash+1:]
}

// QualifierFor returns the name an import binds in the importing file.
//
// By default that is the last segment of the path — `import <semiconductors/bjt>`
// binds `bjt` — because that is what a reader would call it, and it matches
// Go and Python. An explicit `as` overrides it.
//
// Hyphens become underscores so a package name that is not a legal
// identifier still has a usable default: `hvr-rc` binds as `hvr_rc`. Go
// solves the same problem with a `package` clause inside every file; a
// mechanical conversion is less machinery for the same result, and `as` is
// there when the automatic answer is ugly.
func QualifierFor(importPath, alias string) string {
	if alias != "" {
		return alias
	}
	s := strings.Trim(importPath, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 { // an unqualified `<idx:pkg>`
		s = s[i+1:]
	}
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// validQualifier reports why a binding name could not be written as an
// identifier, or nil if it can. The returned error is a fragment ("starts
// with a digit"), meant to be embedded in a sentence naming the import.
func validQualifier(q string) error {
	if q == "" {
		return fmt.Errorf("is empty")
	}
	if c := q[0]; c >= '0' && c <= '9' {
		return fmt.Errorf("starts with a digit")
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' ||
			(c >= '0' && c <= '9')
		if !ok {
			return fmt.Errorf("contains %q", string(c))
		}
	}
	return nil
}

// missingPackageHint returns a diagnostic for an import that failed because
// its package is not installed, or "" when the failure is an ordinary
// missing directory that the generic error describes better.
//
// Needed because a package import and a standard-library import are spelled
// identically: `import <hvr-rc>` failing must not report "no such directory
// .../stdlib/hvr-rc", a path nobody wrote and a directory nobody
// expected to exist. The test is whether the first segment names anything in
// the standard library at all — if it does not, the user meant a package.
func missingPackageHint(importPath string, isSystem bool) string {
	if !isSystem {
		return ""
	}
	pkg, _ := splitPackagePath(importPath)
	if pkg == "" {
		return ""
	}
	if _, installed := PackageRoots[pkg]; installed {
		return "" // installed, so the missing thing is a directory inside it
	}
	if isQualified(pkg) {
		idx, bare, _ := strings.Cut(pkg, ":")
		return fmt.Sprintf("package %q from index %q is not installed — run `hover hpm install %s`",
			bare, idx, pkg)
	}
	if root, err := stdlibRoot(); err == nil {
		if info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(pkg))); statErr == nil && info.IsDir() {
			return "" // a real stdlib directory; the missing thing is inside it
		}
	}
	// Standard-library names get their own message. Releases ship no
	// stdlib, so the overwhelmingly likely cause of `import <math>` failing
	// is a fresh install that has never run --setup — and telling that user
	// to `hover hpm install math` would send them to configure a project
	// when nothing is wrong with their project.
	if isStdlibPackage(pkg) {
		return fmt.Sprintf("the standard library is not installed, so %q cannot be found — "+
			"run `hover --setup` (it downloads the standard library; this needs network access)", pkg)
	}
	// Installed, yet not visible: this file is inside a project, and a project
	// sees only what its own hover.toml declares. Sending this user to install
	// it again would not change anything — the fix is to declare it, from the
	// project directory.
	if isMachineInstalled(pkg) {
		return fmt.Sprintf("package %q is installed on this machine but is not a dependency of the "+
			"project this file belongs to, so it is not visible here — run `hover hpm install %s` "+
			"from the project directory to declare it (a project sees only its own dependencies, "+
			"so that a build of it works the same on any machine)", pkg, pkg)
	}
	return fmt.Sprintf("package %q is not installed — run `hover hpm install %s` "+
		"(or check the path, if you meant a standard library directory)", pkg, pkg)
}

func isQualified(pkg string) bool {
	i := strings.IndexByte(pkg, ':')
	return i > 0 && i < len(pkg)-1
}

// PackageOf returns the package name a system import resolved through, or ""
// when it fell back to the standard library.
func PackageOf(importPath string, isSystem bool) string {
	if !isSystem {
		return ""
	}
	pkg, _ := splitPackagePath(importPath)
	if pkg == "" {
		return ""
	}
	if _, installed := PackageRoots[pkg]; installed || isQualified(pkg) {
		return pkg
	}
	return ""
}

// HvrFilesIn lists the .hvr files directly inside dir, sorted.
//
// Sorted because it decides declaration order across a directory, and
// therefore the C names codegen assigns and the order it emits them — two
// builds of the same source have to produce the same sim.cpp, and directory
// iteration order is not a property of the source.
//
// Deliberately NOT recursive. A subdirectory is a separate import unit, as
// in Go and Python; recursing would make `import <semiconductors>` silently
// pull in every device model in the tree.
func HvrFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".hvr") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
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
