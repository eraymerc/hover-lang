package semantic

import (
	"fmt"
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

// ==========================================
// ANALYZER
// ==========================================

type Analyzer struct {
	currentScope  *Scope
	Errors        []string
	inLogicBlock  bool
	currentDomain token.Type

	// importAliases holds the names introduced by `import "x.hvr" as N;` in
	// the entry file. A dotted expression whose left side is one of these is
	// a qualified reference (`M.sin`), not member access on a variable
	// called M — see checkExpression's "." case, which is the only consumer.
	importAliases map[string]bool

	// structs holds every struct type declared so far — entry file plus
	// imports (see RegisterImportedStructs) — keyed by bare name. Struct
	// types are not alias-qualifiable (see RegisterImportedStructs), so
	// unlike importAliases this is a single flat namespace.
	structs map[string]*StructInfo
}

func NewAnalyzer() *Analyzer {
	globalScope := NewScope(nil)
	globalScope.Define(&Symbol{Name: "time", Type: ast.TDouble})
	globalScope.Define(&Symbol{Name: "dt", Type: ast.TDouble})
	globalScope.Define(&Symbol{Name: "gnd", Type: ast.TWire})
	globalScope.Define(&Symbol{Name: "V", Type: ast.TFunc})
	globalScope.Define(&Symbol{Name: "I", Type: ast.TFunc})
	globalScope.Define(&Symbol{Name: "idt", Type: ast.TFunc})
	globalScope.Define(&Symbol{Name: "ddt", Type: ast.TFunc})
	globalScope.Define(&Symbol{Name: "nr_prev", Type: ast.TFunc})
	return &Analyzer{
		currentScope:  globalScope,
		Errors:        []string{},
		inLogicBlock:  false,
		currentDomain: token.ILLEGAL,
		importAliases: map[string]bool{},
		structs:       map[string]*StructInfo{},
	}
}

// RegisterImportedFunctions pre-registers every top-level FuncDeclStatement
// found in an imported file's AST into the analyzer's global scope, BEFORE
// Analyze() walks the entry file.
//
// Without this, a function declared in an imported file (e.g. a Hover-
// native helper like limexp, imported via `import "./modules/limexp.hvr";`)
// is invisible to the semantic analyzer entirely: FuncDeclStatement only
// registers itself into currentScope as the analyzer walks top-to-bottom
// THROUGH THE SAME FILE (see the *ast.FuncDeclStatement case in
// statements.go) — there is no cross-file propagation. This is genuinely
// different from how the elaborator handles imports (elaborator/types.go's
// Functions/AliasedFunctions maps are built from EVERY imported file, not
// just the entry file), so a function could elaborate and codegen
// correctly while still being rejected here as "Undeclared variable" —
// confirmed directly: a Hover source file importing and calling its own
// plain (non-built-in) function from another file failed semantic checking
// with exactly that error before this fix.
//
// Call this once per imported file's Program, before calling Analyze on
// the entry file's Program.
func (a *Analyzer) RegisterImportedFunctions(importedProgram *ast.Program) {
	for _, stmt := range importedProgram.Statements {
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			a.currentScope.Define(&Symbol{Name: f.Name, Type: ast.TFunc})
		}
	}
}

// RegisterAliasedFunctions is the `import "x.hvr" as N;` form: it registers
// the imported file's functions under their qualified spelling, `N.name`.
//
// Without this, calling an aliased function was rejected here even though
// the elaborator resolved it correctly and codegen emitted it — the
// analyzer saw `M.sin(x)` as member access on an undeclared variable `M`
// and reported two errors for one perfectly valid call. Aliased MODULE
// instantiation never had this problem, because module references do not go
// through expression checking, which is why the gap survived: it only
// showed up for a library that exported functions and was imported with a
// name. Packages make that combination ordinary.
func (a *Analyzer) RegisterAliasedFunctions(alias string, importedProgram *ast.Program) {
	if alias == "" {
		return
	}
	a.importAliases[alias] = true
	for _, stmt := range importedProgram.Statements {
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			a.currentScope.Define(&Symbol{Name: alias + "." + f.Name, Type: ast.TFunc})
		}
	}
}

// RegisterSelectedFunctions is the `from <path> import a, b as c;` form of
// RegisterImportedFunctions: it registers only the named declarations, and
// under the local name each was bound to.
//
// locals maps a declared name to the name it is reachable as in the
// importing file (equal when there is no `as`). Names in locals that turn
// out to be modules rather than functions are skipped rather than reported:
// this pass only exists to stop the entry-file semantic check from calling
// a real function "undeclared", and the elaborator — which can see both
// namespaces — is what actually rejects a name that exists in neither.
func (a *Analyzer) RegisterSelectedFunctions(importedProgram *ast.Program, locals map[string]string) {
	for _, stmt := range importedProgram.Statements {
		f, ok := stmt.(*ast.FuncDeclStatement)
		if !ok {
			continue
		}
		if local, wanted := locals[f.Name]; wanted {
			a.currentScope.Define(&Symbol{Name: local, Type: ast.TFunc})
		}
	}
}

func (a *Analyzer) Analyze(program *ast.Program) []string {
	for _, stmt := range program.Statements {
		a.checkStatement(stmt)
	}
	return a.Errors
}

func (a *Analyzer) addError(node ast.Node, msg string) {
	a.Errors = append(a.Errors, fmt.Sprintf("Line %d: Semantic Error near '%s': %s", node.Line(), node.TokenLiteral(), msg))
}
