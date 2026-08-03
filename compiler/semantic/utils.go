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

// physArity is the expected number of [] entries per primitive, INCLUDING the
// trailing sense-element reference for the current-controlled sources.
//
// Enforcing this is what turns `CCCS<2>() [a, b];` from a silently dead stamp
// (codegen used to emit a "sense element unknown" comment and move on) into an
// actual compile error. Every usage in examples/ and standard_library/ already
// conforms to this table.
//
// Keyed by the spelling the parser accepts (isPhysicalPrimitive); the SPICE
// single-letter aliases R/C/L/V/I/E/G/F/H that codegen tolerates are not
// parseable as statements today, so they are absent here on purpose.
var physArity = map[string]int{
	"R": 2, "C": 2, "L": 2,
	"voltage_source": 2, "current_source": 2,
	"VCVS": 4, "VCCS": 4,
	"CCVS": 3, "CCCS": 3,
}

// bodyStatements safely unwraps a possibly-nil block, mirroring the nil guard
// ModuleDeclStatement.String() already carries — a module whose body failed to
// parse must not turn a name pre-pass into a nil dereference.
func bodyStatements(b *ast.BlockStatement) []ast.Statement {
	if b == nil {
		return nil
	}
	return b.Body
}

// describeSymbol renders a symbol's kind for name-collision diagnostics.
func describeSymbol(s *Symbol) string {
	switch {
	case s.Type.IsElement():
		return "circuit element"
	case s.Type.IsWire():
		return "wire"
	case s.Type.Base == "module":
		return "module"
	case s.Type.Base == "func":
		return "function"
	default:
		return "variable of type '" + s.Type.String() + "'"
	}
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
