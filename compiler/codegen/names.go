package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"strconv"
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
	var b strings.Builder
	b.WriteByte('v')
	for _, seg := range strings.Split(name, ".") {
		b.WriteString(strconv.Itoa(len(seg)))
		b.WriteString(seg)
	}
	return b.String()
}

// cStr emits a C string literal for a net name (used in api_V / api_I calls).
func cStr(name string) string {
	return `"` + name + `"`
}

// ─────────────────────────────────────────────────────────────────────────────
// QUALIFIED NAME HANDLING (aliased imports — Alias.Thing)
// ─────────────────────────────────────────────────────────────────────────────

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

// resolveFunction looks up fnName (bare "sineWave" or qualified
// "Motor.sineWave") in the function namespace of the file that declared the
// statement being emitted, and returns the declaration's compilation identity.
//
// The scope is logic.File, NOT the entry file: a module body in a library
// resolves its calls against that library's own imports. Falling back to
// EntryFile covers a LogicObject with no recorded origin (the single-file
// path, where both are "").
func (g *generator) resolveFunction(fnName string, logic elaborator.LogicObject) *elaborator.FunctionInfo {
	scope, ok := g.prog.FuncScopes[logic.File]
	if !ok {
		scope, ok = g.prog.FuncScopes[g.prog.EntryFile]
		if !ok {
			return nil
		}
	}
	return scope[fnName]
}

// lookupFunctionDecl is resolveFunction for callers that only need the
// declaration (parameter/return types, IsExtern) and not the C name.
func (g *generator) lookupFunctionDecl(fnName string, logic elaborator.LogicObject) *ast.FuncDeclStatement {
	if info := g.resolveFunction(fnName, logic); info != nil {
		return info.Decl
	}
	return nil
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
