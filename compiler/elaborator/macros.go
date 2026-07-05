package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"hover/compiler/token"
)

// processAnalogIdt dynamically rewrites idt(expr) calls into physical 1-Farad capacitors
// processAnalogIdt dynamically rewrites idt() calls into physical capacitors
func (e *Elaborator) processAnalogIdt(module *ast.ModuleDeclStatement) {
	if module.Body == nil {
		return
	}

	idtCounter := 0
	var newBody []ast.Statement
	var injectedStmts []ast.Statement

	var walkExpr func(expr ast.Expression) ast.Expression
	walkExpr = func(expr ast.Expression) ast.Expression {
		if expr == nil {
			return nil
		}

		switch node := expr.(type) {
		case *ast.BinaryExpression:
			node.Left = walkExpr(node.Left)
			node.Right = walkExpr(node.Right)
			return node

		case *ast.CallExpression:
			if id, ok := node.Function.(*ast.IdentifierExpression); ok && id.Value == "idt" && len(node.Arguments) == 1 {

				hiddenNode := fmt.Sprintf("__hidden_idt_%d_%s", idtCounter, module.Name)
				idtCounter++

				assignStmt := &ast.AssignmentStatement{
					Token: module.Token,
					Left:  &ast.IdentifierExpression{Token: token.Token{Type: token.IDENT, Literal: hiddenNode}, Value: hiddenNode},
					Right: node.Arguments[0],
				}

				capStmt := &ast.PhysicalPrimitiveStatement{
					Token:    module.Token,
					PrimType: "C",
					StaticArgs: []ast.Expression{
						&ast.NumberExpression{
							Token: token.Token{Type: token.NUMBER, Literal: "1.0"},
							Value: "1.0",
						},
					},
					PhysArgs: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: "gnd"},
							Value: "gnd",
						},
					},
				}

				csStmt := &ast.PhysicalPrimitiveStatement{
					Token:     module.Token,
					PrimType:  "current_source",
					LogicArgs: []ast.Expression{assignStmt.Left},
					PhysArgs: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: "gnd"},
							Value: "gnd",
						},
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
					},
				}

				injectedStmts = append(injectedStmts, assignStmt, capStmt, csStmt)

				return &ast.CallExpression{
					Token: module.Token,
					Function: &ast.IdentifierExpression{
						Token: token.Token{Type: token.IDENT, Literal: "V"},
						Value: "V",
					},
					Arguments: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
					},
				}
			}

			for i, arg := range node.Arguments {
				node.Arguments[i] = walkExpr(arg)
			}
			return node
		}
		return expr
	}

	for _, stmt := range module.Body.Body {
		injectedStmts = nil

		switch s := stmt.(type) {
		case *ast.ExpressionStatement:
			s.Expression = walkExpr(s.Expression)
		case *ast.AssignmentStatement:
			s.Right = walkExpr(s.Right)
		case *ast.LocalDeclStatement:
			for _, d := range s.Decls {
				d.Value = walkExpr(d.Value)
			}
		}

		if len(injectedStmts) > 0 {
			newBody = append(newBody, injectedStmts...)
		}
		newBody = append(newBody, stmt)
	}

	module.Body.Body = newBody
}

// processAnalogDdt dynamically rewrites ddt(expr) calls into physical
// 1-Henry inductors -- the exact dual of processAnalogIdt's capacitor trick.
//
// An inductor's defining equation is V = L * dI/dt, where I is the current
// THROUGH it. By injecting expr as a current source into a node whose only
// other connection is the inductor (to ground), KCL forces the inductor's
// actual current to equal expr exactly -- so the inductor's voltage becomes
// L * d(expr)/dt, i.e. the time derivative of expr, scaled by L. With L=1,
// V(hiddenNode) = d(expr)/dt directly.
//
// Like idt(), this performs NO numerical differentiation at all -- it lets
// the MNA solver compute the derivative as a natural consequence of solving
// the inductor's own physical equation, inheriting whatever stability and
// accuracy properties the active solver already has for inductors.
func (e *Elaborator) processAnalogDdt(module *ast.ModuleDeclStatement) {
	if module.Body == nil {
		return
	}

	ddtCounter := 0
	var newBody []ast.Statement
	var injectedStmts []ast.Statement

	var walkExpr func(expr ast.Expression) ast.Expression
	walkExpr = func(expr ast.Expression) ast.Expression {
		if expr == nil {
			return nil
		}

		switch node := expr.(type) {
		case *ast.BinaryExpression:
			node.Left = walkExpr(node.Left)
			node.Right = walkExpr(node.Right)
			return node

		case *ast.CallExpression:
			if id, ok := node.Function.(*ast.IdentifierExpression); ok && id.Value == "ddt" && len(node.Arguments) == 1 {

				hiddenNode := fmt.Sprintf("__hidden_ddt_%d_%s", ddtCounter, module.Name)
				ddtCounter++

				assignStmt := &ast.AssignmentStatement{
					Token: module.Token,
					Left:  &ast.IdentifierExpression{Token: token.Token{Type: token.IDENT, Literal: hiddenNode}, Value: hiddenNode},
					Right: node.Arguments[0],
				}

				indStmt := &ast.PhysicalPrimitiveStatement{
					Token:    module.Token,
					PrimType: "L",
					StaticArgs: []ast.Expression{
						&ast.NumberExpression{
							Token: token.Token{Type: token.NUMBER, Literal: "1.0"},
							Value: "1.0",
						},
					},
					PhysArgs: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: "gnd"},
							Value: "gnd",
						},
					},
				}

				csStmt := &ast.PhysicalPrimitiveStatement{
					Token:     module.Token,
					PrimType:  "current_source",
					LogicArgs: []ast.Expression{assignStmt.Left},
					PhysArgs: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: "gnd"},
							Value: "gnd",
						},
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
					},
				}

				injectedStmts = append(injectedStmts, assignStmt, indStmt, csStmt)

				return &ast.CallExpression{
					Token: module.Token,
					Function: &ast.IdentifierExpression{
						Token: token.Token{Type: token.IDENT, Literal: "V"},
						Value: "V",
					},
					Arguments: []ast.Expression{
						&ast.IdentifierExpression{
							Token: token.Token{Type: token.IDENT, Literal: hiddenNode},
							Value: hiddenNode,
						},
					},
				}
			}

			for i, arg := range node.Arguments {
				node.Arguments[i] = walkExpr(arg)
			}
			return node
		}
		return expr
	}

	for _, stmt := range module.Body.Body {
		injectedStmts = nil

		switch s := stmt.(type) {
		case *ast.ExpressionStatement:
			s.Expression = walkExpr(s.Expression)
		case *ast.AssignmentStatement:
			s.Right = walkExpr(s.Right)
		case *ast.LocalDeclStatement:
			for _, d := range s.Decls {
				d.Value = walkExpr(d.Value)
			}
		}

		if len(injectedStmts) > 0 {
			newBody = append(newBody, injectedStmts...)
		}
		newBody = append(newBody, stmt)
	}

	module.Body.Body = newBody
}
