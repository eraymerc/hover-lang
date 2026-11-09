package elaborator

import (
	"strconv"

	"hover/compiler/ast"
	"hover/compiler/token"
)

// processAnalogStateOps tags every ddt(expr) and idt(expr) in an analog
// module's body with its call-site number: ddt(expr) becomes ddt(expr, k).
// Codegen turns a tagged call into sim.ddt(slot, expr) / sim.idt(slot, expr),
// one runtime slot per call site per module instance, and the runtime
// integrates it with the active solver's own formula
// (runtime/vm/state_slots.hpp).
//
// Until 0.9 this pass rewrote the calls into circuit elements instead — ddt(q)
// into a hidden node with a 1 H inductor fed by a current source of value q,
// idt(x) into a hidden node with a 1 F capacitor — so that the solver would
// integrate them. It did, but q became an unknown of the MNA system: a junction
// charge of ~1e-10 C on a row scaled by 1/dt, read back as the difference of
// two nearly equal charges times 1/dt. Halving dt made that worse, which is
// how examples/MOSFET/h_bridge_inverter.hvr failed. The slots keep the
// delegation (the solver's formula, implicit, inside Newton) without the extra
// unknowns.
//
// Only the module body's top-level statements are searched, as before: a ddt()
// inside an if/while body would be evaluated on some steps and not others, and
// a derivative needs its history on every accepted step. An untagged call that
// reaches codegen is reported there.
func (e *Elaborator) processAnalogStateOps(module *ast.ModuleDeclStatement) {
	if module.Body == nil {
		return
	}
	site := 0

	var walk func(expr ast.Expression) ast.Expression
	walk = func(expr ast.Expression) ast.Expression {
		switch node := expr.(type) {
		case *ast.BinaryExpression:
			node.Left = walk(node.Left)
			node.Right = walk(node.Right)
		case *ast.UnaryExpression:
			node.Right = walk(node.Right)
		case *ast.IndexExpression:
			node.Left = walk(node.Left)
			node.Index = walk(node.Index)
		case *ast.CallExpression:
			for i, arg := range node.Arguments {
				node.Arguments[i] = walk(arg)
			}
			if id, ok := node.Function.(*ast.IdentifierExpression); ok &&
				(id.Value == "ddt" || id.Value == "idt") && len(node.Arguments) == 1 {
				lit := strconv.Itoa(site)
				site++
				node.Arguments = append(node.Arguments, &ast.NumberExpression{
					Token: token.Token{Type: token.NUMBER, Literal: lit, Line: id.Token.Line},
					Value: lit,
				})
			}
		}
		return expr
	}

	for _, stmt := range module.Body.Body {
		switch s := stmt.(type) {
		case *ast.ExpressionStatement:
			s.Expression = walk(s.Expression)
		case *ast.AssignmentStatement:
			s.Right = walk(s.Right)
		case *ast.LocalDeclStatement:
			for _, d := range s.Decls {
				d.Value = walk(d.Value)
			}
		}
	}
}
