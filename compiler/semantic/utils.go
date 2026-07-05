package semantic

import (
	ast "hover/compiler/ast"
	"strings"
)

func isNumeric(t string) bool {
	base := getBaseType(t)
	switch base {
	case "double", "int", "float", "number", "unknown":
		return true
	}
	return strings.HasPrefix(base, "unsigned")
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
// parse as NumberExpression → "number", and rejecting those would
// incorrectly flag every bitwise literal operand as an error.
func isFloatingType(t string) bool {
	base := getBaseType(t)
	return base == "double" || base == "float"
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

func getBaseType(t string) string {
	if idx := strings.Index(t, "["); idx != -1 {
		t = t[:idx]
	}
	return strings.TrimRight(t, "*")
}

// stripOneArrayDim removes the OUTERMOST array dimension, mirroring what one
// `[i]` does in C: "double[2][2]" -> "double[2]" -> "double".
func stripOneArrayDim(t string) string {
	open := strings.Index(t, "[")
	if open == -1 {
		return t
	}
	closeIdx := strings.Index(t[open:], "]")
	if closeIdx == -1 {
		return t
	}
	closeIdx += open
	return t[:open] + t[closeIdx+1:]
}
