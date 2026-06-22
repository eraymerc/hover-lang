package semantic

import (
	"fmt"
	ast "hover/Interpreter/ast"
	token "hover/Interpreter/token"
)

// ==========================================
// ANALYZER
// ==========================================

type Analyzer struct {
	currentScope  *Scope
	Errors        []string
	inLogicBlock  bool
	currentDomain token.Type
}

func NewAnalyzer() *Analyzer {
	globalScope := NewScope(nil)
	globalScope.Define(&Symbol{Name: "time", Type: "double"})
	globalScope.Define(&Symbol{Name: "dt", Type: "double"})
	globalScope.Define(&Symbol{Name: "gnd", Type: "wire"})
	globalScope.Define(&Symbol{Name: "sin", Type: "func"})
	globalScope.Define(&Symbol{Name: "cos", Type: "func"})
	globalScope.Define(&Symbol{Name: "V", Type: "func"})
	globalScope.Define(&Symbol{Name: "I", Type: "func"})
	globalScope.Define(&Symbol{Name: "idt", Type: "func"})
	globalScope.Define(&Symbol{Name: "ddt", Type: "func"})
	globalScope.Define(&Symbol{Name: "nr_prev", Type: "func"})
	return &Analyzer{currentScope: globalScope, Errors: []string{}, inLogicBlock: false, currentDomain: token.ILLEGAL}
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
			a.currentScope.Define(&Symbol{Name: f.Name, Type: "func"})
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
