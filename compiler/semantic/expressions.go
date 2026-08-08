package semantic

import (
	"fmt"
	ast "hover/compiler/ast"
)

// qualifiedImportName recognises `Alias.name`, where Alias was introduced by
// an aliased import in the entry file, and returns the qualified spelling
// that RegisterAliasedFunctions defined.
//
// Deliberately narrow: it fires only when the left side is a bare identifier
// that is a KNOWN alias. Anything else — `node.port`, `main.vin`, a real
// member access — falls through to the ordinary operand-by-operand path
// unchanged, so this cannot change the meaning of an expression that already
// worked.
func (a *Analyzer) qualifiedImportName(node *ast.BinaryExpression) (string, bool) {
	left := leftIdentName(node)
	right := rightIdentName(node)
	if left == "" || right == "" || !a.importAliases[left] {
		return "", false
	}
	return left + "." + right, true
}

func leftIdentName(node *ast.BinaryExpression) string {
	if id, ok := node.Left.(*ast.IdentifierExpression); ok {
		return id.Value
	}
	return ""
}

func rightIdentName(node *ast.BinaryExpression) string {
	if id, ok := node.Right.(*ast.IdentifierExpression); ok {
		return id.Value
	}
	return ""
}

func (a *Analyzer) checkExpression(exp ast.Expression) ast.Type {
	if exp == nil {
		return ast.TUnknown
	}
	switch node := exp.(type) {
	case *ast.IdentifierExpression:
		if sym, ok := a.currentScope.Resolve(node.Value); ok {
			return sym.Type
		}
		a.addError(node, fmt.Sprintf("Undeclared variable '%s'", node.Value))
		return ast.TUnknown
	case *ast.NumberExpression:
		return ast.TNumber
	case *ast.BinaryExpression:
		// A dotted name whose left side is an import alias is one qualified
		// reference, not member access on a variable. Checked BEFORE the
		// operands, since checking them individually is exactly what
		// produced "Undeclared variable 'M'" for a valid `M.sin(x)`.
		if node.Operator == "." {
			if name, ok := a.qualifiedImportName(node); ok {
				if sym, ok := a.currentScope.Resolve(name); ok {
					return sym.Type
				}
				a.addError(node, fmt.Sprintf(
					"'%s' does not declare '%s'", leftIdentName(node), rightIdentName(node)))
				return ast.TUnknown
			}
		}

		l := a.checkExpression(node.Left)
		r := a.checkExpression(node.Right)

		if node.Operator == "." {
			return r
		}

		if l.IsWire() || r.IsWire() {
			a.addError(node, "Cannot perform math on physical wires. Use V() or I() to read their values.")
			return ast.TUnknown
		}

		if isBitwiseOp(node.Operator) {
			// Real C/C++ semantics: &, |, ^, <<, >> require integer operands.
			// double_var & 5 is a compile error in C ("invalid operands to
			// binary &"), not an implicit truncation — Hover preserves that
			// rather than silently converting, matching the explicit-cast
			// philosophy used everywhere else in this type system.
			if isFloatingType(l) {
				a.addError(node, fmt.Sprintf(
					"Invalid operands to binary '%s' — bitwise operators require integer types, got '%s' on the left. "+
						"Cast explicitly with (int) if truncation is intended.",
					node.Operator, l))
			}
			if isFloatingType(r) {
				a.addError(node, fmt.Sprintf(
					"Invalid operands to binary '%s' — bitwise operators require integer types, got '%s' on the right. "+
						"Cast explicitly with (int) if truncation is intended.",
					node.Operator, r))
			}
		}

		return l
	case *ast.UnaryExpression:
		r := a.checkExpression(node.Right)
		if r.IsWire() {
			a.addError(node, "Cannot perform math on a physical wire. Use V() or I() to read its value.")
		}
		if node.Operator == "~" && isFloatingType(r) {
			a.addError(node, fmt.Sprintf(
				"Invalid operand to unary '~' — bitwise NOT requires an integer type, got '%s'. "+
					"Cast explicitly with (int) if truncation is intended.",
				r))
		}
		if node.Operator == "&" {
			return r.AddrOf()
		}
		if node.Operator == "*" {
			// One dereference removes one pointer level. (The old
			// string-based code stripped ALL decorations at once via
			// getBaseType — **pp dereferenced once now correctly yields a
			// pointer instead of the bare element type.)
			return r.Deref()
		}
		return r
	case *ast.CallExpression:
		a.checkExpression(node.Function)
		for _, arg := range node.Arguments {
			a.checkExpression(arg)
		}
		return ast.TDouble
	case *ast.IndexExpression:
		lType := a.checkExpression(node.Left)
		iType := a.checkExpression(node.Index)
		if !isNumeric(iType) && !iType.IsWire() {
			a.addError(node, fmt.Sprintf("Array index must be numeric, got '%s'", iType))
		}
		if !lType.IsArray() && lType.Stars == 0 &&
			lType.Base != "unknown" && !lType.IsWire() {
			a.addError(node, fmt.Sprintf("Cannot index non-array type '%s'", lType))
		}
		// One subscript removes the outermost array dimension (or one
		// pointer level when indexing a pointer — the old code returned
		// the pointer type unchanged, which was wrong).
		return lType.IndexOnce()
	}
	return ast.TUnknown
}
