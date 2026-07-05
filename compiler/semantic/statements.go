package semantic

import (
	"fmt"
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

func (a *Analyzer) checkStatement(stmt ast.Statement) {
	switch node := stmt.(type) {

	case *ast.LocalDeclStatement:
		if a.currentDomain == token.MODULE {
			for _, decl := range node.Decls {
				if !isStructuralInit(decl.Value) {
					a.addError(node, fmt.Sprintf(
						"'%s' — structural modules cannot compute values. Move math into an analog or digital module.",
						decl.Name,
					))
				}
			}
		}

		if a.currentDomain == token.MODULE && node.Type != "wire" && !node.IsState {
			for _, decl := range node.Decls {
				a.addError(node, fmt.Sprintf(
					"'%s' — variables in structural modules must be declared as 'state'. "+
						"Plain variables reset every step; use 'state double %s = 0' instead.",
					decl.Name, decl.Name,
				))
			}
		}

		if a.currentDomain == token.ANALOG && node.IsState {
			a.addError(node, "Analog modules cannot have 'state' variables — analog models are stateless.")
		}

		for _, decl := range node.Decls {
			if decl.Value != nil {
				a.checkExpression(decl.Value)
			}
			a.currentScope.Define(&Symbol{Name: decl.Name, Type: node.Type, IsState: node.IsState})
		}
	case *ast.AssignmentStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain math or logic. They are only for wiring components together.")
		}
		a.checkExpression(node.Left)
		a.checkExpression(node.Right)
	case *ast.BlockStatement:
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, s := range node.Body {
			a.checkStatement(s)
		}
		a.currentScope = parent
	case *ast.IfStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain 'if' statements.")
		}

		condType := a.checkExpression(node.Condition)
		if condType == "wire" {
			a.addError(node, "Condition expression cannot be a physical wire. Use V() or I() to read its value.")
		}

		prev := a.inLogicBlock
		a.inLogicBlock = true
		a.checkStatement(node.Consequence)
		for _, alt := range node.Alternatives {
			a.checkExpression(alt.Condition)
			a.checkStatement(alt.Body)
		}
		if node.Alternative != nil {
			a.checkStatement(node.Alternative)
		}
		a.inLogicBlock = prev
	case *ast.WhileStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain 'while' loops.")
		}
		if a.currentDomain == token.ANALOG {
			a.addError(node, "Analog modules cannot contain 'while' loops — risk of infinite loop inside Newton-Raphson convergence.")
		}
		a.checkExpression(node.Condition)

		condType := a.checkExpression(node.Condition)
		if condType == "wire" {
			a.addError(node, "Condition expression cannot be a physical wire.")
		}

		a.checkStatement(node.Body)
	case *ast.ReturnStatement:
		retType := a.checkExpression(node.ReturnValue)
		if retType == "wire" {
			a.addError(node, "Functions cannot return physical wires.")
		}
	case *ast.ModuleDeclStatement:
		a.currentScope.Define(&Symbol{Name: node.Name, Type: "module"})
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, arg := range node.StaticArgs {
			a.currentScope.Define(&Symbol{Name: arg.Name, Type: arg.Type})
		}
		for _, arg := range node.LogicArgs {
			a.currentScope.Define(&Symbol{Name: arg.Name, Type: arg.Type})
		}
		for _, port := range node.PhysPorts {
			a.currentScope.Define(&Symbol{Name: port, Type: "wire"})
		}

		prevDomain := a.currentDomain
		a.currentDomain = node.Token.Type

		a.checkStatement(node.Body)

		a.currentDomain = prevDomain
		a.currentScope = parent
	case *ast.FuncDeclStatement:
		a.currentScope.Define(&Symbol{Name: node.Name, Type: "func"})
		if node.Body == nil { // extern: nothing to analyze
			return
		}
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, p := range node.Parameters {
			a.currentScope.Define(&Symbol{Name: p.Name, Type: p.Type})
		}
		a.checkStatement(node.Body)
		a.currentScope = parent
	case *ast.ModuleInstStatement, *ast.PhysicalPrimitiveStatement:
		if _, isPhys := node.(*ast.PhysicalPrimitiveStatement); isPhys && a.currentDomain == token.DIGITAL {
			a.addError(node, "Digital modules cannot instantiate analog hardware (like R, C, or voltage_source).")
		}

		var physArgs []ast.Expression
		if mi, ok := stmt.(*ast.ModuleInstStatement); ok {
			physArgs = mi.PhysArgs
		}
		if pp, ok := stmt.(*ast.PhysicalPrimitiveStatement); ok {
			physArgs = pp.PhysArgs
		}

		for _, arg := range physArgs {
			t := a.checkExpression(arg)
			if t != "wire" && t != "unknown" {
				a.addError(stmt, fmt.Sprintf("Physical port must be a wire, got '%s'", t))
			}
		}
	}
}
