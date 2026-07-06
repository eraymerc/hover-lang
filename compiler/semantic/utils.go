package semantic

import (
	ast "hover/compiler/ast"
	"strings"
)

// isNumeric reports whether a value of type t can be used where a number is
// expected. Deliberately checks the BASE only (matching the old string-based
// behavior, which stripped array/pointer decorations first): an array or
// pointer of a numeric base still passes, and rejecting those is left to the
// specific contexts that care (indexing, conditions).
func isNumeric(t ast.Type) bool {
	switch t.Base {
	case "double", "int", "float", "number", "unknown":
		return true
	}
	return strings.HasPrefix(t.Base, "unsigned")
}

// isBitwiseOp reports whether op is one of the bitwise/shift operators
// (&, |, ^, <<, >>) that require integer operands in real C/C++ semantics.
// Unary ~ is checked separately in checkExpression's UnaryExpression case
// since it has no left operand to inspect here.
func isBitwiseOp(op string) bool {
	switch op {
	case "&", "|", "^", "<<", ">>":
		return true
	}
	return false
}

// isFloatingType reports whether t is a floating-point type that real C
// would reject as an operand to a bitwise operator. "number" (untyped
// literal) and "unknown" (already-errored expression) are deliberately
// NOT considered floating here — literals like the integer constant `5`
// parse as NumberExpression -> TNumber, and rejecting those would
// incorrectly flag every bitwise literal operand as an error.
func isFloatingType(t ast.Type) bool {
	return t.Base == "double" || t.Base == "float"
}

// isStructuralInit returns true if an expression is simple enough
// for a structural module signal wire — literal, MNA read, or plain identifier.
func isStructuralInit(expr ast.Expression) bool {
	if expr == nil {
		return true
	}
	switch expr.(type) {
	case *ast.NumberExpression:
		return true
	case *ast.IdentifierExpression:
		return true
	case *ast.CallExpression:
		return true
	}
	return false
}
