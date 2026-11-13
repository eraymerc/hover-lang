package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"hover/compiler/token"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// TIME BREAKPOINTS — comparisons of `time` against a threshold
//
// Logic samples at the start of each step and holds. A pulse
//
//     if (time >= t0 && time < t0 + w) { i = amp; }
//
// shorter than the step that contains it is never sampled, and .zcd, which
// compares the drive at a step's two ends, cannot see it either. SPICE lands
// its steps on a PULSE source's corners (breakpoints); here the compiler finds
// the corners in the code. Every comparison
//
//     S(time)  >  >=  <  <=  X          (either way round)
//
// whose threshold X does not move within a step becomes sim.tcmp(form, X, c,
// T): the same value, computed by the same floating-point operations, plus a
// report of when its result next flips. The runtime ends a step exactly on
// that instant (runtime/vm/breakpoints.hpp). S(time) is one of
//
//     time,  time + c,  c + time,  time - c
//     fmod(u, T),  u % T,  u - floor(u / T) * T        u one of the above
//
// — a monotone time, or a sawtooth of it (pulse trains: `fmod(time, T) < w`
// flips at k T and k T + w).
//
// "Does not move within a step" (stepInvariant): X, c and T are built from
// literals, parameters, extern (math) calls and variables that nothing assigns
// from a time-varying expression. A variable assigned from `time`, `dt`, a
// user function call or — in analog code, which runs at every Newton
// iteration — V(), I(), ddt(), idt(), nr_prev() varies (bpVarying, a fixed
// point over every assignment and every argument passed to a function
// parameter). A `state` variable of logic does NOT vary for this purpose even
// when assigned from `time`: it is written once, at a step's start, and holds
// — which is exactly how a dead-time module remembers when a gate turned off
// (`t_off = time; ... time - t_off >= t_dead`), and that threshold is exact.
//
// Not recognised (left to .zcd, as before): any other function of time
// (`sin(w*time) > 0`, `time * f > n`), a time expression held in a variable
// first (`double ph = time * f; ... ph - floor(ph) < d`) or passed into a
// function parameter, == and !=, thresholds that vary within a step, and
// comparisons in clocked modules (they only sample at their ticks anyway).
// ─────────────────────────────────────────────────────────────────────────────

// bpLogicPlaceholder marks the top of phase_structural and phase_digital.
// Whether the deck has logic breakpoints is only known once every phase has
// been emitted, so finishBreakpoints fills it in afterwards.
func bpLogicPlaceholder(fnName string) string { return "/*@@HVR_BP_" + fnName + "@@*/" }

// finishBreakpoints: in a deck with breakpoints in its logic, the runtime runs
// the step's logic pass itself, at the start of each step attempt, to collect
// them (runtime/vm/breakpoints.cpp); the solver's own call of the logic phases
// in that attempt then returns at once. Every other deck gets nothing — the
// placeholder line is removed, so its sim.cpp is what it was before.
func (g *generator) finishBreakpoints() {
	out := g.sb.String()
	for k, fn := range []string{"phase_structural", "phase_digital"} {
		mark := bpLogicPlaceholder(fn) + "\n"
		repl := ""
		if g.bpLogicSites > 0 {
			repl = fmt.Sprintf("    if (sim.logic_ran_early(%d)) return;  // breakpoints: this pass already ran (runtime/vm/breakpoints.hpp)\n", k)
		}
		out = strings.Replace(out, mark, repl, 1)
	}
	g.sb.Reset()
	g.sb.WriteString(out)
}

// isBuiltinTime: `time` meaning the simulation time, not a parameter or port
// of the same name (resolveIdent checks those first).
func isBuiltinTime(e ast.Expression, logic elaborator.LogicObject) bool {
	id, ok := e.(*ast.IdentifierExpression)
	if !ok || id.Value != "time" {
		return false
	}
	if _, p := logic.Params[id.Value]; p {
		return false
	}
	_, port := logic.Ports[id.Value]
	return !port
}

// externNamed: a call of the <math> extern `name` (fmod, floor) — the C
// function itself, which the runtime calls too.
func (g *generator) externNamed(e ast.Expression, name string, nargs int, logic elaborator.LogicObject) (*ast.CallExpression, bool) {
	call, ok := e.(*ast.CallExpression)
	if !ok || len(call.Arguments) != nargs {
		return nil, false
	}
	info := g.resolveFunction(callExpressionName(call.Function), logic)
	if info == nil || !info.Decl.IsExtern || info.CName != name {
		return nil, false
	}
	return call, true
}

// timeSide matches S(time). u: 0 time, 1 time + c, 2 time - c; o: 0 none,
// 1 fmod(u, T), 2 u % T, 3 u - floor(u / T) * T.
type timeSide struct {
	u, o int
	c, p ast.Expression
}

func (g *generator) matchU(e ast.Expression, logic elaborator.LogicObject) (int, ast.Expression, bool) {
	if isBuiltinTime(e, logic) {
		return 0, nil, true
	}
	b, ok := e.(*ast.BinaryExpression)
	if !ok {
		return 0, nil, false
	}
	switch b.Operator {
	case "+":
		if isBuiltinTime(b.Left, logic) && g.stepInvariant(b.Right, logic) {
			return 1, b.Right, true
		}
		if isBuiltinTime(b.Right, logic) && g.stepInvariant(b.Left, logic) {
			return 1, b.Left, true
		}
	case "-":
		if isBuiltinTime(b.Left, logic) && g.stepInvariant(b.Right, logic) {
			return 2, b.Right, true
		}
	}
	return 0, nil, false
}

func (g *generator) matchTimeSide(e ast.Expression, logic elaborator.LogicObject) (timeSide, bool) {
	if u, c, ok := g.matchU(e, logic); ok {
		return timeSide{u: u, c: c}, true
	}
	if call, ok := g.externNamed(e, "fmod", 2, logic); ok {
		if u, c, ok := g.matchU(call.Arguments[0], logic); ok && g.stepInvariant(call.Arguments[1], logic) {
			return timeSide{u: u, c: c, o: 1, p: call.Arguments[1]}, true
		}
		return timeSide{}, false
	}
	b, ok := e.(*ast.BinaryExpression)
	if !ok {
		return timeSide{}, false
	}
	switch b.Operator {
	case "%":
		if u, c, ok := g.matchU(b.Left, logic); ok && g.stepInvariant(b.Right, logic) {
			return timeSide{u: u, c: c, o: 2, p: b.Right}, true
		}
	case "-":
		// u - floor(u / T) * T, the product either way round.
		u, c, ok := g.matchU(b.Left, logic)
		prod, pok := b.Right.(*ast.BinaryExpression)
		if !ok || !pok || prod.Operator != "*" {
			break
		}
		for _, side := range [][2]ast.Expression{{prod.Left, prod.Right}, {prod.Right, prod.Left}} {
			fl, ok := g.externNamed(side[0], "floor", 1, logic)
			if !ok {
				continue
			}
			div, ok := fl.Arguments[0].(*ast.BinaryExpression)
			if !ok || div.Operator != "/" || div.Left.String() != b.Left.String() ||
				div.Right.String() != side[1].String() || !g.stepInvariant(div.Right, logic) {
				continue
			}
			return timeSide{u: u, c: c, o: 3, p: div.Right}, true
		}
	}
	return timeSide{}, false
}

// emitTimeCompare emits a recognised comparison as sim.tcmp, or reports false
// to leave it to the ordinary emission.
func (g *generator) emitTimeCompare(n *ast.BinaryExpression, logic elaborator.LogicObject) (string, bool) {
	// Not in the AD copy (time is a constant to Newton there; the plain copy
	// reports), not inside nr_prev(...), not in a clocked module.
	if g.ad != nil || g.usingPrevAPI || logic.Clock != "" {
		return "", false
	}
	ops := map[string]int{">": 0, ">=": 1, "<": 2, "<=": 3}
	flip := map[string]string{">": "<", ">=": "<=", "<": ">", "<=": ">="}
	op, ok := ops[n.Operator]
	if !ok {
		return "", false
	}
	side, threshold := ast.Expression(nil), ast.Expression(nil)
	var ts timeSide
	if t, ok := g.matchTimeSide(n.Left, logic); ok && g.stepInvariant(n.Right, logic) {
		ts, side, threshold = t, n.Left, n.Right
	} else if t, ok := g.matchTimeSide(n.Right, logic); ok && g.stepInvariant(n.Left, logic) {
		ts, side, threshold = t, n.Right, n.Left // X op S  is  S flip(op) X
		op = ops[flip[n.Operator]]
	}
	if side == nil {
		return "", false
	}
	asDouble := func(e ast.Expression) string {
		if e == nil {
			return "0.0"
		}
		code, t := g.emitExpr(e, logic)
		return emitCast(code, t, CDouble)
	}
	form := op | ts.u<<2 | ts.o<<4
	g.bpSites++
	if g.inFunction > 0 || logic.Domain != token.ANALOG {
		g.bpLogicSites++
	}
	return fmt.Sprintf("sim.tcmp(%d, %s, %s, %s)", form, asDouble(threshold), asDouble(ts.c), asDouble(ts.p)), true
}

// stepInvariant: e has one value for the whole of a step (see the header).
func (g *generator) stepInvariant(e ast.Expression, logic elaborator.LogicObject) bool {
	analog := logic.Domain == token.ANALOG || g.inFunction > 0
	return g.invariantIn(e, logic, analog, g.bpVarying())
}

func (g *generator) invariantIn(e ast.Expression, logic elaborator.LogicObject, analog bool, varying map[string]bool) bool {
	switch n := e.(type) {
	case nil:
		return true
	case *ast.NumberExpression:
		return true
	case *ast.IdentifierExpression:
		if _, ok := logic.Params[n.Value]; ok {
			return true
		}
		if mangled, ok := logic.Ports[n.Value]; ok {
			return !varying[mangle(mangled)]
		}
		if n.Value == "time" || n.Value == "dt" {
			return false
		}
		return !varying[mangle(logic.Prefix+"."+n.Value)]
	case *ast.UnaryExpression:
		return n.Operator != "*" && n.Operator != "&" && g.invariantIn(n.Right, logic, analog, varying)
	case *ast.BinaryExpression:
		if n.Operator == "." {
			if n.IsFieldAccess {
				return g.invariantIn(n.Left, logic, analog, varying)
			}
			return g.invariantIn(n.Right, logic, analog, varying)
		}
		return g.invariantIn(n.Left, logic, analog, varying) && g.invariantIn(n.Right, logic, analog, varying)
	case *ast.IndexExpression:
		return g.invariantIn(n.Left, logic, analog, varying) && g.invariantIn(n.Index, logic, analog, varying)
	case *ast.CallExpression:
		switch callExpressionName(n.Function) {
		case "V", "I":
			// Logic reads the solution at the step's start and holds it; analog
			// code reads every Newton iterate.
			return !analog
		case "ddt", "idt", "nr_prev":
			return false
		case "nodiff":
			return len(n.Arguments) == 1 && g.invariantIn(n.Arguments[0], logic, analog, varying)
		}
		info := g.resolveFunction(callExpressionName(n.Function), logic)
		if info == nil || !info.Decl.IsExtern {
			return false // a user function may read time
		}
		for _, a := range n.Arguments {
			if !g.invariantIn(a, logic, analog, varying) {
				return false
			}
		}
		return true
	}
	return false
}

// bpVarying is the set of variables (mangled) that can change within a step:
// assigned, somewhere, from a time-varying expression in the assigning code's
// own context. Computed once, as a fixed point.
func (g *generator) bpVarying() map[string]bool {
	if g.bpVary != nil {
		return g.bpVary
	}
	type assign struct {
		target string
		rhs    ast.Expression
		logic  elaborator.LogicObject
		analog bool
	}
	var assigns []assign

	// `state` variables of logic hold from one step to the next.
	logicState := map[string]bool{}
	for _, l := range g.prog.Logic {
		if d, ok := l.Source.(*ast.LocalDeclStatement); ok && d.IsState && l.Domain != token.ANALOG {
			for _, v := range d.Decls {
				logicState[resolveWrite(v.Name, l)] = true
			}
		}
	}

	var walkExpr func(e ast.Expression, logic elaborator.LogicObject, analog bool)
	walkExpr = func(e ast.Expression, logic elaborator.LogicObject, analog bool) {
		switch n := e.(type) {
		case *ast.BinaryExpression:
			walkExpr(n.Left, logic, analog)
			walkExpr(n.Right, logic, analog)
		case *ast.UnaryExpression:
			walkExpr(n.Right, logic, analog)
		case *ast.IndexExpression:
			walkExpr(n.Left, logic, analog)
			walkExpr(n.Index, logic, analog)
		case *ast.CallExpression:
			for _, a := range n.Arguments {
				walkExpr(a, logic, analog)
			}
			// An argument is an assignment to the callee's parameter.
			info := g.resolveFunction(callExpressionName(n.Function), logic)
			if info != nil && !info.Decl.IsExtern {
				for i, a := range n.Arguments {
					if i < len(info.Decl.Parameters) {
						assigns = append(assigns, assign{mangle(info.CName + "." + info.Decl.Parameters[i].Name), a, logic, analog})
					}
				}
			}
		}
	}
	baseName := func(e ast.Expression, logic elaborator.LogicObject) (string, bool) {
		for {
			switch n := e.(type) {
			case *ast.IdentifierExpression:
				return resolveWrite(n.Value, logic), true
			case *ast.IndexExpression:
				e = n.Left
			case *ast.BinaryExpression:
				if n.Operator != "." {
					return "", false
				}
				e = n.Left
			case *ast.UnaryExpression:
				e = n.Right
			default:
				return "", false
			}
		}
	}
	var walk func(s ast.Statement, logic elaborator.LogicObject, analog bool)
	walk = func(s ast.Statement, logic elaborator.LogicObject, analog bool) {
		switch n := s.(type) {
		case *ast.BlockStatement:
			if n != nil {
				for _, c := range n.Body {
					walk(c, logic, analog)
				}
			}
		case *ast.IfStatement:
			if n == nil {
				return
			}
			walkExpr(n.Condition, logic, analog)
			walk(n.Consequence, logic, analog)
			for _, alt := range n.Alternatives {
				walkExpr(alt.Condition, logic, analog)
				walk(alt.Body, logic, analog)
			}
			if n.Alternative != nil {
				walk(n.Alternative, logic, analog)
			}
		case *ast.WhileStatement:
			if n != nil {
				walkExpr(n.Condition, logic, analog)
				walk(n.Body, logic, analog)
			}
		case *ast.LocalDeclStatement:
			if n == nil || n.Type.IsWire() || n.IsState {
				return // a state initialiser runs once, at t = 0
			}
			for _, d := range n.Decls {
				if d.Value != nil {
					walkExpr(d.Value, logic, analog)
					assigns = append(assigns, assign{resolveWrite(d.Name, logic), d.Value, logic, analog})
				}
			}
		case *ast.AssignmentStatement:
			if n == nil {
				return
			}
			walkExpr(n.Right, logic, analog)
			if t, ok := baseName(n.Left, logic); ok {
				assigns = append(assigns, assign{t, n.Right, logic, analog})
			}
		case *ast.ReturnStatement:
			if n != nil {
				walkExpr(n.ReturnValue, logic, analog)
			}
		case *ast.ExpressionStatement:
			if n != nil {
				walkExpr(n.Expression, logic, analog)
			}
		}
	}
	for _, l := range g.prog.Logic {
		walk(l.Source, l, l.Domain == token.ANALOG)
	}
	for _, fn := range g.prog.Functions {
		if fn.Decl.IsExtern {
			continue
		}
		fl := elaborator.LogicObject{Prefix: fn.CName, Params: map[string]float64{}, Ports: map[string]string{}, File: fn.File}
		for _, p := range fn.Decl.Parameters {
			fl.Ports[p.Name] = fn.CName + "." + p.Name
		}
		walk(fn.Decl.Body, fl, true)
	}

	varying := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, a := range assigns {
			if varying[a.target] || (logicState[a.target] && !a.analog) {
				continue
			}
			if !g.invariantIn(a.rhs, a.logic, a.analog, varying) {
				varying[a.target] = true
				changed = true
			}
		}
	}
	g.bpVary = varying
	return varying
}
