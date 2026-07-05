package elaborator

import (
	"fmt"
	"hover/Interpreter/ast"
	"math"
	"sort"
	"strings"
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

// evalStatic evaluates a compile-time-constant expression (a static module
// argument like <1k, 2*R_load>) down to a float64. It reports rather than
// swallows every failure mode: an identifier that isn't a parameter in
// scope, a division by zero, an operator outside the supported set, and
// any expression form that has no meaning in a static context (function
// calls, array literals, indexing, ...). On any error the reported value
// is 0 — but because the error lands in e.errors, elaboration fails and
// that 0 never reaches the generated simulation.
func (e *Elaborator) evalStatic(exp ast.Expression, params map[string]float64) float64 {
	if exp == nil {
		return 0
	}
	switch n := exp.(type) {

	case *ast.NumberExpression:
		return ParseEngineering(n.Value)

	case *ast.IdentifierExpression:
		val, ok := params[n.Value]
		if !ok {
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: unknown identifier '%s' in static expression — %s",
				n.Line(), n.Value, availableParamsHint(params)))
			return 0
		}
		return val

	case *ast.BinaryExpression:
		l := e.evalStatic(n.Left, params)
		r := e.evalStatic(n.Right, params)
		switch n.Operator {
		case "+":
			return l + r
		case "-":
			return l - r
		case "*":
			return l * r
		case "/":
			if r == 0 {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: division by zero in static expression '%s'",
					n.Line(), exp.String()))
				return 0
			}
			return l / r
		case "**":
			return math.Pow(l, r)
		default:
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: unsupported operator '%s' in static expression '%s' — "+
					"static arguments support only + - * / **",
				n.Line(), n.Operator, exp.String()))
			return 0
		}

	case *ast.UnaryExpression:
		val := e.evalStatic(n.Right, params)
		if n.Operator == "-" {
			return -val
		}
		e.errors = append(e.errors, fmt.Sprintf(
			"Line %d: unsupported unary operator '%s' in static expression '%s' — "+
				"only unary '-' is allowed",
			n.Line(), n.Operator, exp.String()))
		return 0
	}

	// Any other node kind (CallExpression, ArrayExpression, IndexExpression,
	// ...) cannot be evaluated at elaboration time.
	e.errors = append(e.errors, fmt.Sprintf(
		"Line %d: expression '%s' cannot be evaluated at elaboration time — "+
			"static arguments must be built from numeric literals, parameter names, "+
			"and + - * / ** arithmetic",
		exp.Line(), exp.String()))
	return 0
}

// availableParamsHint renders the set of parameter names currently in scope
// for a static expression, sorted for deterministic error output. Shown
// whenever an unknown identifier is reported, so a typo'd parameter name
// ('resistence') is immediately answerable from the error message itself.
func availableParamsHint(params map[string]float64) string {
	if len(params) == 0 {
		return "no parameters are in scope here"
	}
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	return "available parameters: " + strings.Join(names, ", ")
}
