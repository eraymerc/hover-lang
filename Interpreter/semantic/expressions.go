package semantic

import (
	"fmt"
	ast "hover/Interpreter/ast"
	"strings"
)

func (a *Analyzer) checkExpression(exp ast.Expression) string {
	if exp == nil {
		return "unknown"
	}
	switch node := exp.(type) {
	case *ast.IdentifierExpression:
		if sym, ok := a.currentScope.Resolve(node.Value); ok {
			return sym.Type
		}
		a.addError(node, fmt.Sprintf("Undeclared variable '%s'", node.Value))
		return "unknown"
	case *ast.NumberExpression:
		return "number"
	case *ast.BinaryExpression:
		l := a.checkExpression(node.Left)
		r := a.checkExpression(node.Right)

		if node.Operator == "." {
			return r
		}

		if l == "wire" || r == "wire" {
			a.addError(node, "Cannot perform math on physical wires. Use V() or I() to read their values.")
			return "unknown"
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
		if r == "wire" {
			a.addError(node, "Cannot perform math on a physical wire. Use V() or I() to read its value.")
		}
		if node.Operator == "~" && isFloatingType(r) {
			a.addError(node, fmt.Sprintf(
				"Invalid operand to unary '~' — bitwise NOT requires an integer type, got '%s'. "+
					"Cast explicitly with (int) if truncation is intended.",
				r))
		}
		return r
	case *ast.CallExpression:
		a.checkExpression(node.Function)
		for _, arg := range node.Arguments {
			a.checkExpression(arg)
		}
		return "double"
	case *ast.IndexExpression:
		lType := a.checkExpression(node.Left)
		iType := a.checkExpression(node.Index)
		if !isNumeric(iType) && iType != "wire" {
			a.addError(node, fmt.Sprintf("Array index must be numeric, got '%s'", iType))
		}

		if !strings.Contains(lType, "[") && lType != "unknown" && lType != "wire" {
			a.addError(node, fmt.Sprintf("Cannot index non-array type '%s'", lType))
		}
		return getBaseType(lType)
	}
	return "unknown"
}
