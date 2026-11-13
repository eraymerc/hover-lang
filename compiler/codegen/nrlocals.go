package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"hover/compiler/token"
)

// ─────────────────────────────────────────────────────────────────────────────
// nr_prev(x) OF AN ANALOG VARIABLE
//
// nr_prev(V(a) - V(b)) has always meant the expression at the previous Newton
// iterate's node voltages (usingPrevAPI, expressions.go). A variable inside
// nr_prev(...) now means the same thing for the variable: x as the evaluation
// at the previous Newton iterate left it. Its use is SPICE's junction-limiter
// anchor, the device's own previously LIMITED voltage:
//
//     double vl = pnjlim(V(a) - V(b), nr_prev(vl), nvt, vcrit);
//
// (the one self-reference the analyzer admits — semantic/analyzer.go). Every
// variable an analog module body assigns and some nr_prev(...) reads gets one
// runtime slot per module instance, keyed by its mangled name, like the ddt()
// slots. phase_analog stores it after every assignment (sim.nr_record), so the
// slot holds the variable's last value in that evaluation; nr_prev(x) reads
// sim.nr_local(k). Which evaluation's value that is — the iterate's own, never
// a Jacobian or trust-region probe — the runtime works out
// (runtime/vm/nr_locals.hpp). The AD copy of phase_analog reads the same
// value as a plain double, so it carries no derivative, and records nothing.
//
// Not a variable, so unchanged inside nr_prev: module parameters, `time`,
// `dt`, and inputs no analog code writes — logic is sample-and-hold, constant
// across the iterations of a step.
//
// Not allowed: a variable inside nr_prev in a FUNCTION body (one C function
// serves every caller, so its locals have no instance to keep a slot per — a
// model passes nr_prev(x) from its own body into the helper instead, as every
// junction model passes it into pnjlim), and in a digital module (it runs once
// per step, outside Newton).
// ─────────────────────────────────────────────────────────────────────────────

// nrSlot returns the slot nr_prev(name) reads in logic, allocating it on first
// use, or ok == false when name is not an analog-written variable (it is then
// emitted as itself). Reports the forbidden uses.
func (g *generator) nrSlot(name string, logic elaborator.LogicObject) (int, bool) {
	if _, isParam := logic.Params[name]; isParam || name == "time" || name == "dt" {
		return 0, false
	}
	if g.inFunction > 0 {
		g.errors = append(g.errors, fmt.Sprintf(
			"nr_prev(%s): a variable inside nr_prev() is only allowed in an analog module body, "+
				"not in a function — a function has no module instance to keep its previous value for. "+
				"Pass nr_prev(%s) into the function from the module instead", name, name))
		return 0, false
	}
	if logic.Domain != token.ANALOG || logic.Clock != "" {
		g.errors = append(g.errors, fmt.Sprintf(
			"nr_prev(%s): a variable inside nr_prev() is only allowed in an analog module — "+
				"digital logic runs once per step, outside the Newton iteration", name))
		return 0, false
	}
	target := resolveWrite(name, logic)
	if !g.analogWritten()[target] {
		return 0, false
	}
	if ht := g.typeOf(target); !ht.isScalar() || ht.elem == CStruct {
		g.errors = append(g.errors, fmt.Sprintf(
			"nr_prev(%s): only a scalar variable can be read at the previous Newton iterate", name))
		return 0, false
	}
	if g.nrSlots == nil {
		g.nrSlots = map[string]int{}
	}
	k, seen := g.nrSlots[target]
	if !seen {
		k = len(g.nrSlots)
		g.nrSlots[target] = k
	}
	return k, true
}

// emitNrRecord stores target into its slot after an assignment, while
// phase_analog is being emitted.
func (g *generator) emitNrRecord(target string) {
	if !g.recordNr {
		return
	}
	if k, ok := g.nrSlots[target]; ok {
		g.line("sim.nr_record(%d, %s);", k, emitCast(target, g.typeOf(target).elem, CDouble))
	}
}

// emitPhaseAnalog emits phase_analog. The slots are found while emitting it,
// so a write emitted before the first nr_prev(x) that reads it would go
// unrecorded: when the first pass finds any, the phase is emitted again with
// every slot known, opening with sim.nr_begin().
func (g *generator) emitPhaseAnalog() {
	start, nErr := g.sb.Len(), len(g.errors)
	g.recordNr = true
	g.emitPhaseByDomain(token.ANALOG, "phase_analog", "")
	if len(g.nrSlots) > 0 {
		all := g.sb.String()
		g.sb.Reset()
		g.sb.WriteString(all[:start])
		g.errors = g.errors[:nErr]
		g.emitPhaseByDomain(token.ANALOG, "phase_analog", "")
	}
	g.recordNr = false
}

// analogWritten: every global an analog module body assigns (cached).
func (g *generator) analogWritten() map[string]bool {
	if g.nrWrites != nil {
		return g.nrWrites
	}
	g.nrWrites = map[string]bool{}
	var walk func(s ast.Statement, logic elaborator.LogicObject)
	walk = func(s ast.Statement, logic elaborator.LogicObject) {
		switch n := s.(type) {
		case *ast.BlockStatement:
			if n != nil {
				for _, c := range n.Body {
					walk(c, logic)
				}
			}
		case *ast.IfStatement:
			if n != nil {
				walk(n.Consequence, logic)
				for _, alt := range n.Alternatives {
					walk(alt.Body, logic)
				}
				walk(n.Alternative, logic)
			}
		case *ast.WhileStatement:
			if n != nil {
				walk(n.Body, logic)
			}
		case *ast.LocalDeclStatement:
			if n != nil && !n.Type.IsWire() {
				for _, d := range n.Decls {
					g.nrWrites[resolveWrite(d.Name, logic)] = true
				}
			}
		case *ast.AssignmentStatement:
			if n != nil {
				if id, ok := n.Left.(*ast.IdentifierExpression); ok {
					g.nrWrites[resolveWrite(id.Value, logic)] = true
				}
			}
		}
	}
	for _, logic := range g.prog.Logic {
		if logic.Domain == token.ANALOG && logic.Clock == "" {
			walk(logic.Source, logic)
		}
	}
	return g.nrWrites
}
