package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"strings"
)

// dottedPath extracts a dotted name path ("main.q1_base") from an
// expression STRUCTURALLY — IdentifierExpression or a chain of "."
// BinaryExpressions — returning ok=false for anything else. This replaces
// cleanString(expr.String()): round-tripping the AST through the debug
// pretty-printer ("(main . q1_base)") and scrubbing the punctuation back
// out was a shadow parser, and it silently "succeeded" on expressions
// that were never valid name paths at all.
func dottedPath(expr ast.Expression) (string, bool) {
	switch e := expr.(type) {
	case *ast.IdentifierExpression:
		return e.Value, true
	case *ast.BinaryExpression:
		if e.Operator != "." {
			return "", false
		}
		left, ok := dottedPath(e.Left)
		if !ok {
			return "", false
		}
		right, rok := e.Right.(*ast.IdentifierExpression)
		if !rok {
			return "", false
		}
		return left + "." + right.Value, true
	}
	return "", false
}

// mangle converts a dotted mangled name to a valid C++ identifier.
// "main.ctrl_pid.integral" → "main_ctrl_pid_integral"
func mangle(name string) string {
	return strings.ReplaceAll(name, ".", "__")
}

// cStr emits a C string literal for a net name (used in api_V / api_I calls).
func cStr(name string) string {
	return `"` + name + `"`
}

// ─────────────────────────────────────────────────────────────────────────────
// QUALIFIED NAME HANDLING (aliased imports — Alias.Thing)
// ─────────────────────────────────────────────────────────────────────────────

// splitQualifiedCallName splits "Motor.sineWave" into ("Motor", "sineWave", true),
// or returns ("", "sineWave", false) for an unqualified "sineWave". Mirrors
// elaborator.splitQualifiedName — duplicated locally since codegen doesn't
// otherwise depend on elaborator's internal (unexported) helpers.
func splitQualifiedCallName(name string) (alias string, bare string, isQualified bool) {
	idx := strings.IndexByte(name, '.')
	if idx == -1 {
		return "", name, false
	}
	return name[:idx], name[idx+1:], true
}

// callExpressionName extracts the real callable name from a CallExpression's
// Function field, handling both bare calls (sineWave(...), where Function is
// an *ast.IdentifierExpression) and qualified calls from aliased imports
// (Motor.sineWave(...), where Function is an *ast.BinaryExpression with "."
// as the operator).
//
// n.Function.TokenLiteral() is NOT sufficient for the qualified case — for a
// BinaryExpression, TokenLiteral() returns the "." token's own literal ("."),
// not the dotted name. This walks the structure directly instead.
func callExpressionName(fn ast.Expression) string {
	switch f := fn.(type) {
	case *ast.IdentifierExpression:
		return f.Value
	case *ast.BinaryExpression:
		if f.Operator == "." {
			return callExpressionName(f.Left) + "." + callExpressionName(f.Right)
		}
	}
	return fn.TokenLiteral()
}

// resolveFunctionCName looks up fnName (bare "sineWave" or qualified
// "Motor.sineWave") against the elaborated program's function tables and
// returns the mangled C identifier to use at the call site, plus whether
// it was found at all.
//
// The returned name is always mangled (dots replaced with underscores) so
// that two aliases each contributing a function with the same bare name
// (Motor.sineWave and Aux.sineWave) emit as distinct C functions
// (Motor_sineWave, Aux_sineWave) rather than colliding.
func (g *generator) resolveFunctionCName(fnName string) (string, bool) {
	alias, bare, isQualified := splitQualifiedCallName(fnName)
	if !isQualified {
		if _, ok := g.prog.Functions[fnName]; ok {
			return fnName, true // bare functions keep their plain name
		}
		return "", false
	}
	if byName, ok := g.prog.AliasedFunctions[alias]; ok {
		if _, ok := byName[bare]; ok {
			return mangle(alias + "." + bare), true
		}
	}
	return "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// LOGIC-OBJECT NAME RESOLUTION
// Resolves identifiers/writes/net-names within the context of a single
// LogicObject (its static Params, its Ports mapping, and its Prefix).
// ─────────────────────────────────────────────────────────────────────────────

// resolveIdent resolves an identifier in the context of a LogicObject.
// Mirrors Go VM: resolveIdent — checks params, ports, then prefix.
func resolveIdent(name string, logic elaborator.LogicObject) string {
	// Static param → emit as literal
	if val, ok := logic.Params[name]; ok {
		return fmt.Sprintf("%.17g", val)
	}
	// Port mapping → mangled signal name
	if mangled, ok := logic.Ports[name]; ok {
		return mangle(mangled)
	}
	// Special keywords
	if name == "time" {
		return "vm->time"
	}
	if name == "dt" {
		return "vm->time_step"
	}
	// Local variable
	return mangle(logic.Prefix + "." + name)
}

// resolveWrite resolves the LHS write target for a name in a LogicObject.
func resolveWrite(name string, logic elaborator.LogicObject) string {
	if mangled, ok := logic.Ports[name]; ok {
		return mangle(mangled)
	}
	return mangle(logic.Prefix + "." + name)
}

// resolveNet resolves a net name for V()/I() calls.
func resolveNet(name string, logic elaborator.LogicObject) string {
	if name == "gnd" || name == "0" {
		return "gnd"
	}
	if mangled, ok := logic.Ports[name]; ok {
		return mangled
	}
	return logic.Prefix + "." + name
}
