package codegen

import (
	ast "hover/Interpreter/ast"
	"hover/Interpreter/elaborator"
)

// ─────────────────────────────────────────────────────────────────────────────
// STATEMENT EMITTER
//
// Now that emitExpr returns a CType alongside its emitted code, every
// assignment and local declaration here inserts an explicit C++ cast when
// the RHS expression's inferred type differs from the LHS's declared
// type — e.g. `int x = some_double_expr;` emits `main_x = (int64_t)(...)`.
// ─────────────────────────────────────────────────────────────────────────────

// emitStmt recursively translates a Hover statement into C++ statements,
// writing directly into g.sb via g.line/g.raw.
//
// inFunc distinguishes two emission contexts for LocalDeclStatement:
//   - inFunc == true  (inside a user-defined function body): declare a
//     true C local variable, since function-local names are scoped to
//     that single C function and never referenced from elsewhere.
//   - inFunc == false (inside a phase body): the variable was already
//     declared as a file-scope static by collectAllVars/emitStateVars,
//     so only emit a plain assignment — redeclaring it here would shadow
//     the global and break visibility across phase-function boundaries.
func (g *generator) emitStmt(stmt ast.Statement, logic elaborator.LogicObject, inFunc bool) {
	if stmt == nil {
		return
	}

	switch s := stmt.(type) {

	case *ast.LocalDeclStatement:
		if s.Type == "wire" {
			return // wires are not runtime variables
		}
		declaredType := hoverTypeToCType(s.Type)
		for _, d := range s.Decls {
			target := resolveWrite(d.Name, logic)
			if s.IsState {
				// State: managed by init_state(), skip here
				continue
			}
			if inFunc {
				// Inside a function: declare as a true C local variable,
				// with its real declared type rather than a uniform double.
				if d.Value != nil {
					valCode, valType := g.emitExpr(d.Value, logic)
					valCode = emitCast(valCode, valType, declaredType)
					g.line("%s %s = %s;", declaredType.String(), target, valCode)
				} else {
					g.line("%s %s = 0;", declaredType.String(), target)
				}
			} else {
				// Inside a phase block: variable is already a file-scope
				// global declared with its real type (see emitStateVars in
				// main_emit.go) — emit plain assignment, casting the RHS to
				// match, no re-declaration.
				if d.Value != nil {
					valCode, valType := g.emitExpr(d.Value, logic)
					valCode = emitCast(valCode, valType, declaredType)
					g.line("%s = %s;", target, valCode)
				}
			}
		}

	case *ast.AssignmentStatement:
		lhs := ""
		lhsType := CDouble
		if id, ok := s.Left.(*ast.IdentifierExpression); ok {
			lhs = resolveWrite(id.Value, logic)
			lhsType = g.identifierType(id.Value, logic)
		} else {
			lhsCode, lhsT := g.emitExpr(s.Left, logic)
			lhs = lhsCode
			lhsType = lhsT
		}
		rhsCode, rhsType := g.emitExpr(s.Right, logic)
		rhsCode = emitCast(rhsCode, rhsType, lhsType)
		g.line("%s = %s;", lhs, rhsCode)

	case *ast.IfStatement:
		condCode, condType := g.emitExpr(s.Condition, logic)
		condCode = emitCast(condCode, condType, CDouble)
		g.line("if ((%s) != 0.0) {", condCode)
		g.push()
		g.emitStmt(s.Consequence, logic, inFunc)
		g.pop()

		for _, alt := range s.Alternatives {
			altCode, altType := g.emitExpr(alt.Condition, logic)
			altCode = emitCast(altCode, altType, CDouble)
			g.line("} else if ((%s) != 0.0) {", altCode)
			g.push()
			g.emitStmt(alt.Body, logic, inFunc)
			g.pop()
		}

		if s.Alternative != nil {
			g.line("} else {")
			g.push()
			g.emitStmt(s.Alternative, logic, inFunc)
			g.pop()
		}
		g.line("}")

	case *ast.WhileStatement:
		condCode, condType := g.emitExpr(s.Condition, logic)
		condCode = emitCast(condCode, condType, CDouble)
		g.line("while ((%s) != 0.0) {", condCode)
		g.push()
		g.emitStmt(s.Body, logic, inFunc)
		g.pop()
		g.line("}")

	case *ast.BlockStatement:
		for _, child := range s.Body {
			g.emitStmt(child, logic, inFunc)
		}

	case *ast.ReturnStatement:
		// Cast to the enclosing function's declared return type. logic.Prefix
		// is the function's mangled C name (set in emitOneFunction —
		// functions.go), and the return type comes from looking up that
		// function's declaration the same way calls do.
		valCode, valType := g.emitExpr(s.ReturnValue, logic)
		returnType := g.currentFunctionReturnType(logic)
		valCode = emitCast(valCode, valType, returnType)
		g.line("return %s;", valCode)

	case *ast.ExpressionStatement:
		valCode, _ := g.emitExpr(s.Expression, logic)
		g.line("(void)(%s);", valCode)
	}
}

// currentFunctionReturnType looks up the return type of the function
// currently being emitted, identified by logic.Prefix (the function's
// mangled C name, set by emitOneFunction). Falls back to CDouble if the
// prefix doesn't match any known function — this can legitimately happen
// for ReturnStatement nodes reached outside a function body in contexts
// this compiler doesn't otherwise restrict at parse time; CDouble matches
// the pre-existing behavior before return types were tracked.
func (g *generator) currentFunctionReturnType(logic elaborator.LogicObject) CType {
	for _, fn := range g.prog.Functions {
		if fn.Name == logic.Prefix {
			return hoverTypeToCType(fn.ReturnType)
		}
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			if mangle(alias+"."+fn.Name) == logic.Prefix {
				return hoverTypeToCType(fn.ReturnType)
			}
		}
	}
	return CDouble
}
