package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
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
		if s.Type.IsWire() {
			return
		}
		ht := hoverTypeOf(s.Type)
		for _, d := range s.Decls {
			target := resolveWrite(d.Name, logic)
			if s.IsState {
				continue // initialized at file scope by emitStateVars
			}
			if ht.isArray() {
				if inFunc {
					g.line("%s;", ht.cVarDecl(target))
				}
				if d.Value != nil {
					g.emitArrayInitAssigns(target, ht, d.Value, logic)
				}
				continue
			}
			if inFunc {
				if d.Value != nil {
					valCode, valType := g.emitExpr(d.Value, logic)
					g.line("%s = %s;", ht.cVarDecl(target), castToHover(valCode, valType, ht))
				} else {
					g.line("%s = 0;", ht.cVarDecl(target))
				}
			} else if d.Value != nil {
				valCode, valType := g.emitExpr(d.Value, logic)
				g.line("%s = %s;", target, castToHover(valCode, valType, ht))
			}
		}

	case *ast.AssignmentStatement:
		if id, ok := s.Left.(*ast.IdentifierExpression); ok {
			lhs := resolveWrite(id.Value, logic)
			lhsHT := g.typeOf(lhs)
			rhsCode, rhsType := g.emitExpr(s.Right, logic)
			g.line("%s = %s;", lhs, castToHover(rhsCode, rhsType, lhsHT))
		} else {
			lhsCode, lhsT := g.emitExpr(s.Left, logic)
			rhsCode, rhsType := g.emitExpr(s.Right, logic)
			g.line("%s = %s;", lhsCode, emitCast(rhsCode, rhsType, lhsT))
		}

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

// emitArrayInitAssigns fills a 1-D array element-by-element from a Hover
// initializer (brace list, or scalar-fill). Multidimensional runtime init is
// not flattened here (flat indexing is only valid for 1-D); file-scope
// multidim still gets brace/scalar-fill via formatInitializer.
func (g *generator) emitArrayInitAssigns(target string, ht hoverType, init ast.Expression, logic elaborator.LogicObject) {
	g.emitArrayElemAssigns(target, "", ht.elem, ht.dims, init, logic)
}

// emitArrayElemAssigns recursively emits fully-subscripted per-element
// assignments (target[0][1] = ...), handling nested brace lists and
// scalar-fill at any dimension level. This is what makes in-function/in-phase
// multidimensional initialization work (state-level init goes through
// formatArrayLiteral instead).
func (g *generator) emitArrayElemAssigns(target, subscript string, elem CType, dims []int, init ast.Expression, logic elaborator.LogicObject) {
	if len(dims) == 0 {
		code, t := g.emitExpr(init, logic)
		g.line("%s%s = %s;", target, subscript, emitCast(code, t, elem))
		return
	}
	if arr, ok := init.(*ast.ArrayExpression); ok {
		for i, el := range arr.Elements {
			g.emitArrayElemAssigns(target, fmt.Sprintf("%s[%d]", subscript, i), elem, dims[1:], el, logic)
		}
		return
	}
	for i := 0; i < dims[0]; i++ { // scalar-fill
		g.emitArrayElemAssigns(target, fmt.Sprintf("%s[%d]", subscript, i), elem, dims[1:], init, logic)
	}
}
