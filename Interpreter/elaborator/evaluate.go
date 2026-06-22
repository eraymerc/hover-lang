package elaborator

import (
	"hover/Interpreter/ast"
	"math"
)

func (e *Elaborator) mangleNode(n, prefix string, ports map[string]string) string {
	if n == "gnd" || n == "0" {
		return "gnd"
	}
	if m, ok := ports[n]; ok {
		return m
	}
	return prefix + "." + n
}

func (e *Elaborator) mangleExpression(exp ast.Expression, prefix string, ports map[string]string) string {
	if exp == nil {
		return ""
	}
	switch n := exp.(type) {
	case *ast.IdentifierExpression:
		return e.mangleNode(n.Value, prefix, ports)
	case *ast.BinaryExpression:
		if n.Operator == "." {
			return e.mangleExpression(n.Left, prefix, ports) + "." + n.Right.String()
		}
	}
	return exp.String()
}

func (e *Elaborator) evalStatic(exp ast.Expression, params map[string]float64) float64 {
	if exp == nil {
		return 0
	}
	switch n := exp.(type) {
	case *ast.NumberExpression:
		return ParseEngineering(n.Value)
	case *ast.IdentifierExpression:
		return params[n.Value]
	case *ast.BinaryExpression:
		l, r := e.evalStatic(n.Left, params), e.evalStatic(n.Right, params)
		switch n.Operator {
		case "+":
			return l + r
		case "-":
			return l - r
		case "*":
			return l * r
		case "/":
			if r == 0 {
				return 0
			}
			return l / r
		case "**":
			return math.Pow(l, r)
		}
	case *ast.UnaryExpression:
		val := e.evalStatic(n.Right, params)
		if n.Operator == "-" {
			return -val
		}
		return val
	}
	return 0
}
