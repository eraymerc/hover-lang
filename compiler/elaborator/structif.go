package elaborator

import (
	"fmt"

	ast "hover/compiler/ast"
)

// ─────────────────────────────────────────────────────────────────────────────
// NO CIRCUIT ELEMENTS INSIDE `if` / `while`
//
// The circuit's structure — its MNA rows and stamps — is fixed when the deck is
// compiled, so an element or a module instance cannot depend on a branch.
// Elaboration rejects one inside an `if` or a `while` at any depth.
//
// Until 2026-11-13 such an `if` was emitted as run-time logic, where codegen
// has nothing to do with an element: it vanished without a word and a node
// lost its only path and went to inf (docs/hoveR_sorunlar.txt item 35). To
// choose at run time, keep the element and switch what drives it (a source
// value, a conductance).
// ─────────────────────────────────────────────────────────────────────────────

// isStructure reports whether stmt is itself an element or a module instance.
func isStructure(stmt ast.Statement) bool {
	switch stmt.(type) {
	case *ast.PhysicalPrimitiveStatement, *ast.ModuleInstStatement:
		return true
	}
	return false
}

// containsStructure reports whether a statement is, or holds at any depth, an
// element or a module instance.
func containsStructure(stmt ast.Statement) bool {
	switch s := stmt.(type) {
	case *ast.PhysicalPrimitiveStatement, *ast.ModuleInstStatement:
		return true
	case *ast.BlockStatement:
		return s != nil && blockHasStructure(s.Body)
	case *ast.IfStatement:
		if s.Consequence != nil && blockHasStructure(s.Consequence.Body) {
			return true
		}
		for _, alt := range s.Alternatives {
			if alt.Body != nil && blockHasStructure(alt.Body.Body) {
				return true
			}
		}
		return s.Alternative != nil && blockHasStructure(s.Alternative.Body)
	case *ast.WhileStatement:
		return s.Body != nil && blockHasStructure(s.Body.Body)
	}
	return false
}

func blockHasStructure(stmts []ast.Statement) bool {
	for _, st := range stmts {
		if containsStructure(st) {
			return true
		}
	}
	return false
}

func structureInControlFlowMsg(stmt ast.Statement) string {
	kind := "`if`"
	if _, ok := stmt.(*ast.WhileStatement); ok {
		kind = "`while`"
	}
	return fmt.Sprintf(
		"Line %d: a circuit element or module instance inside %s — not allowed: the circuit's structure "+
			"is fixed when the deck is compiled. Keep the element outside and switch what drives it "+
			"(a source value, a conductance) instead.", stmt.Line(), kind)
}
