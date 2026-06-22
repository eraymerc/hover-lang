package codegen

import (
	"fmt"
	ast "hover/Interpreter/ast"
	"hover/Interpreter/elaborator"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// EXPRESSION EMITTER
//
// emitExpr now returns (code string, type CType) instead of a bare string.
// Every binary/unary operator computes its result type via promote() —
// see ctype.go — following real C/C++ usual arithmetic conversions
// (including truncating integer division and the signed/unsigned
// promotion footgun, preserved faithfully per design decision). Explicit
// C++ casts are inserted at every type boundary: when an operand's type
// differs from what an operator/call/assignment expects, rather than
// relying on C++'s own implicit conversions.
// ─────────────────────────────────────────────────────────────────────────────

// emitExpr recursively translates a Hover expression into a C++ expression
// string plus its inferred CType, in the context of the given LogicObject
// (for identifier/port resolution — see names.go).
func (g *generator) emitExpr(expr ast.Expression, logic elaborator.LogicObject) (string, CType) {
	if expr == nil {
		return "0.0", CDouble
	}

	switch n := expr.(type) {

	case *ast.NumberExpression:
		// Untyped literal — see hoverTypeToCType's "number" case: literals
		// default to CDouble. The C++ literal text itself is still just the
		// numeric value; no suffix is added, since the surrounding context
		// (an assignment, an argument slot) is what may cast it afterward.
		val := elaborator.ParseEngineering(n.Value)
		return fmt.Sprintf("%.17g", val), CDouble

	case *ast.IdentifierExpression:
		code := resolveIdent(n.Value, logic)
		return code, g.identifierType(n.Value, logic)

	case *ast.UnaryExpression:
		innerCode, innerType := g.emitExpr(n.Right, logic)
		switch n.Operator {
		case "-":
			return fmt.Sprintf("(-(%s))", innerCode), innerType
		case "!":
			// Boolean-as-double convention, unrelated to the integer type
			// system — preserved exactly as before.
			return fmt.Sprintf("((%s) == 0.0 ? 1.0 : 0.0)", innerCode), CDouble
		case "~":
			// Semantic analysis already rejects ~ on a float/double operand
			// (see semantic/expressions.go's isFloatingType check), so by
			// the time codegen sees this, innerType is guaranteed CInt or
			// CUInt. No cast needed — bitwise NOT on an already-integer
			// C++ value is just ~ directly.
			return fmt.Sprintf("(~(%s))", innerCode), innerType
		}
		return innerCode, innerType

	case *ast.BinaryExpression:
		// Dot operator — member access, just emit right side
		if n.Operator == "." {
			return g.emitExpr(n.Right, logic)
		}

		lCode, lType := g.emitExpr(n.Left, logic)
		rCode, rType := g.emitExpr(n.Right, logic)

		switch n.Operator {
		case "+", "-", "*":
			resultType := promote(lType, rType, n.Operator)
			lCast := emitCast(lCode, lType, resultType)
			rCast := emitCast(rCode, rType, resultType)
			return fmt.Sprintf("((%s) %s (%s))", lCast, n.Operator, rCast), resultType

		case "/":
			resultType := promote(lType, rType, "/")
			lCast := emitCast(lCode, lType, resultType)
			rCast := emitCast(rCode, rType, resultType)
			if resultType == CInt || resultType == CUInt {
				// Real C truncating integer division — no zero-guard needed
				// since integer division by zero is undefined behavior in
				// C too, not a guarded 0.0 the way the old double-only
				// division was. This matches real C/C++ behavior exactly,
				// per the explicit decision to preserve it rather than add
				// safety nets C itself doesn't have.
				return fmt.Sprintf("((%s) / (%s))", lCast, rCast), resultType
			}
			// Floating division keeps the existing divide-by-zero guard —
			// this was Hover's own safety convention before the type
			// system existed and isn't part of "real C behavior" either
			// way (C's float division by zero produces inf/nan, it
			// doesn't trap) — preserved as-is for floating types only.
			return fmt.Sprintf("((%s) == 0.0 ? 0.0 : (%s) / (%s))", rCast, lCast, rCast), resultType

		case "%":
			resultType := promote(lType, rType, "%")
			lCast := emitCast(lCode, lType, resultType)
			rCast := emitCast(rCode, rType, resultType)
			if resultType == CInt || resultType == CUInt {
				return fmt.Sprintf("((%s) %% (%s))", lCast, rCast), resultType
			}
			return fmt.Sprintf("((%s) == 0.0 ? 0.0 : fmod(%s, %s))", rCast, lCast, rCast), resultType

		case "**":
			// Hover-specific operator, no C equivalent — pow() always
			// operates in double regardless of operand types, matching
			// its existing behavior before this type system existed.
			lCast := emitCast(lCode, lType, CDouble)
			rCast := emitCast(rCode, rType, CDouble)
			return fmt.Sprintf("pow(%s, %s)", lCast, rCast), CDouble

		case "&", "|", "^":
			// Semantic analysis already rejects float/double operands here
			// (see isFloatingType check) — both operands are guaranteed
			// integer-typed by the time codegen sees this.
			resultType := promote(lType, rType, n.Operator)
			lCast := emitCast(lCode, lType, resultType)
			rCast := emitCast(rCode, rType, resultType)
			return fmt.Sprintf("((%s) %s (%s))", lCast, n.Operator, rCast), resultType

		case "<<", ">>":
			// C's actual rule: shift result type follows the LEFT operand
			// only — the right operand (shift count) doesn't participate
			// in promotion. Preserved faithfully per design decision.
			// Semantic analysis already rejects float/double on either
			// side, so lType and rType are both guaranteed integer here.
			return fmt.Sprintf("((%s) %s (%s))", lCode, n.Operator, rCode), lType

		case ">", "<", ">=", "<=", "==", "!=":
			// Boolean-as-double convention — comparisons always return
			// CDouble (1.0/0.0), unrelated to operand types. Operands
			// still need comparing in their common promoted type so e.g.
			// comparing an int and a double compares numerically, not as
			// mismatched C++ types triggering a compiler warning.
			resultType := promote(lType, rType, n.Operator)
			lCast := emitCast(lCode, lType, resultType)
			rCast := emitCast(rCode, rType, resultType)
			return fmt.Sprintf("((%s) %s (%s) ? 1.0 : 0.0)", lCast, n.Operator, rCast), CDouble

		case "&&", "||":
			// Logical operators — boolean-as-double convention, unrelated
			// to operand integer/float typing.
			return fmt.Sprintf("(((%s) != 0.0 %s (%s) != 0.0) ? 1.0 : 0.0)", lCode, n.Operator, rCode), CDouble
		}

	case *ast.CallExpression:
		fnName := callExpressionName(n.Function)
		switch fnName {
		case "V":
			if len(n.Arguments) > 0 {
				net := resolveNet(n.Arguments[0].String(), logic)
				if g.usingPrevAPI {
					return fmt.Sprintf(`api_V_prev(vm->api, %s)`, cStr(net)), CDouble
				}
				return fmt.Sprintf(`api_V(vm->api, %s)`, cStr(net)), CDouble
			}
			return "0.0", CDouble
		case "I":
			if len(n.Arguments) > 0 {
				net := resolveNet(n.Arguments[0].String(), logic)
				if g.usingPrevAPI {
					return fmt.Sprintf(`api_I_prev(vm->api, %s)`, cStr(net)), CDouble
				}
				return fmt.Sprintf(`api_I(vm->api, %s)`, cStr(net)), CDouble
			}
			return "0.0", CDouble
		case "nr_prev":
			// nr_prev(expr) emits expr's full C++ translation with every
			// nested V()/I() call routed to api_V_prev/api_I_prev — "this
			// expression's value as of the previous Newton iteration
			// within the current timestep's solve," NOT the previous
			// timestep's value (that's what `state` already covers).
			// usingPrevAPI is saved/restored around the recursive call so
			// nesting (nr_prev inside an otherwise-normal expression, or
			// even nr_prev inside nr_prev, however unusual) behaves
			// correctly rather than leaking the flag to unrelated
			// sibling expressions emitted afterward.
			if len(n.Arguments) > 0 {
				saved := g.usingPrevAPI
				g.usingPrevAPI = true
				code, ctype := g.emitExpr(n.Arguments[0], logic)
				g.usingPrevAPI = saved
				return code, ctype
			}
			return "0.0", CDouble
		case "sin":
			if len(n.Arguments) > 0 {
				argCode, argType := g.emitExpr(n.Arguments[0], logic)
				return fmt.Sprintf("sin(%s)", emitCast(argCode, argType, CDouble)), CDouble
			}
			return "0.0", CDouble
		case "cos":
			if len(n.Arguments) > 0 {
				argCode, argType := g.emitExpr(n.Arguments[0], logic)
				return fmt.Sprintf("cos(%s)", emitCast(argCode, argType, CDouble)), CDouble
			}
			return "0.0", CDouble
		default:
			return g.emitUserFunctionCall(fnName, n.Arguments, logic)
		}

	case *ast.IndexExpression:
		arrCode, arrType := g.emitExpr(n.Left, logic)
		idxCode, idxType := g.emitExpr(n.Index, logic)
		idxCast := emitCast(idxCode, idxType, CInt)
		return fmt.Sprintf("%s[(int)(%s)]", arrCode, idxCast), arrType
	}

	return "0.0", CDouble
}

// identifierType resolves the CType of an identifier in the context of a
// LogicObject — static params are always CDouble (they're substituted as
// literal float values at elaboration time, see elaborator.evalStatic),
// the special keywords time/dt are CDouble (vm->time / vm->time_step are
// real C++ doubles), and everything else is looked up in the type table
// built by collectVarTypes.
func (g *generator) identifierType(name string, logic elaborator.LogicObject) CType {
	if _, ok := logic.Params[name]; ok {
		return CDouble
	}
	if name == "time" || name == "dt" {
		return CDouble
	}
	mangled := resolveWrite(name, logic)
	return g.typeOf(mangled)
}

// emitUserFunctionCall resolves and emits a call to a user-defined Hover
// function (bare or qualified via an aliased import — see
// resolveFunctionCName in names.go), casting each argument to the
// function's declared parameter type and returning the function's
// declared return type.
func (g *generator) emitUserFunctionCall(fnName string, argExprs []ast.Expression, logic elaborator.LogicObject) (string, CType) {
	cName, ok := g.resolveFunctionCName(fnName)
	fnDecl := g.lookupFunctionDecl(fnName)

	if !ok || fnDecl == nil {
		// Unknown function — emit a clearly broken call rather than
		// silently producing 0.0, so the C++ compiler's "undeclared
		// identifier" error points back to a real Hover-side bug instead
		// of a silently wrong simulation result. Also record the name
		// (deduplicated) so the caller can print a proper Hover-level
		// diagnostic after Generate returns — see GenerateWithDiagnostics.
		alreadyRecorded := false
		for _, seen := range g.unresolvedFunctions {
			if seen == fnName {
				alreadyRecorded = true
				break
			}
		}
		if !alreadyRecorded {
			g.unresolvedFunctions = append(g.unresolvedFunctions, fnName)
		}

		cName = fmt.Sprintf("/* UNRESOLVED_FUNCTION_%s */ %s", mangle(fnName), mangle(fnName))
		args := []string{"vm"}
		for _, arg := range argExprs {
			argCode, _ := g.emitExpr(arg, logic)
			args = append(args, argCode)
		}
		return fmt.Sprintf("%s(%s)", cName, strings.Join(args, ", ")), CDouble
	}

	args := []string{"vm"}
	for i, arg := range argExprs {
		argCode, argType := g.emitExpr(arg, logic)
		if i < len(fnDecl.Parameters) {
			paramType := hoverTypeToCType(fnDecl.Parameters[i].Type)
			argCode = emitCast(argCode, argType, paramType)
		}
		args = append(args, argCode)
	}

	returnType := hoverTypeToCType(fnDecl.ReturnType)
	return fmt.Sprintf("%s(%s)", cName, strings.Join(args, ", ")), returnType
}

// lookupFunctionDecl finds the *ast.FuncDeclStatement for a bare or
// qualified function name, mirroring resolveFunctionCName's bare/aliased
// split but returning the declaration itself (needed for parameter/return
// types) rather than just the mangled C name.
func (g *generator) lookupFunctionDecl(fnName string) *ast.FuncDeclStatement {
	alias, bare, isQualified := splitQualifiedCallName(fnName)
	if !isQualified {
		if fn, ok := g.prog.Functions[fnName]; ok {
			return fn
		}
		return nil
	}
	if byName, ok := g.prog.AliasedFunctions[alias]; ok {
		if fn, ok := byName[bare]; ok {
			return fn
		}
	}
	return nil
}
