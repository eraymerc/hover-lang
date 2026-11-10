package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"hover/compiler/token"
	"regexp"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// AUTOMATIC DIFFERENTIATION — the analog Jacobian
//
// The runtime's Newton solvers need J = -dB/dx, B being what the driven
// sources stamp. Until 0.9 it was always numerical: vm_compute_jacobian
// perturbed each unknown in turn and re-ran every analog block, n+2 full
// evaluations per formation — a third of the run on
// examples/MOSFET/h_bridge_inverter.hvr, and a finite-difference estimate at
// that.
//
// Here the compiler differentiates instead, as Verilog-A compilers do
// (ADMS, OpenVAF). The analog blocks are emitted a second time as
// phase_jacobian, with every `double` replaced by hvr::Dual (forward-mode AD,
// runtime/hover_sim.hpp):
//
//   - every variable an analog block WRITES becomes a local Dual, initialised
//     from its global (so a branch that does not assign it reads the value the
//     double code left, as it would) — the phase never writes a global;
//   - V(n) / I(e) read the unknown with a unit derivative (sim.V_ad), V_prev /
//     I_prev / time / dt stay plain doubles (constants to Newton, exactly as
//     the numerical Jacobian treated them);
//   - ddt / idt use the slot formula's derivative (sim.ddt_ad);
//   - user functions called from analog code get Dual overloads, emitted the
//     same way; <math.h> calls resolve to hvr's Dual overloads by ADL;
//   - at the end, every source driven by an analog-written variable writes its
//     derivative row into the Jacobian (sim.ad_current_source /
//     ad_voltage_source). Sources driven by digital logic are constants.
//
// Digital modules are never differentiated: they run once per step, outside
// Newton. A deck whose analog code the AD copy cannot express gets no
// phase_jacobian and keeps the numerical Jacobian — an extern other than the
// <math.h> set hover_sim.hpp overloads, an array/struct/pointer written by an
// analog block, or a called function with array/pointer/struct parameters.
// ─────────────────────────────────────────────────────────────────────────────

// adMathExterns are the <math.h> externs with Dual overloads in hover_sim.hpp.
var adMathExterns = map[string]bool{
	"sin": true, "cos": true, "tan": true, "asin": true, "acos": true, "atan": true, "atan2": true,
	"sinh": true, "cosh": true, "tanh": true, "asinh": true, "acosh": true, "atanh": true,
	"exp": true, "exp2": true, "expm1": true, "log": true, "log10": true, "log2": true, "log1p": true,
	"ldexp": true, "pow": true, "sqrt": true, "cbrt": true, "hypot": true, "fabs": true,
	"erf": true, "erfc": true, "ceil": true, "floor": true, "trunc": true, "round": true,
	"lround": true, "nearbyint": true, "rint": true, "lrint": true, "fmod": true, "remainder": true,
	"copysign": true, "fdim": true, "fmax": true, "fmin": true, "fma": true,
}

// adState is the bookkeeping of one AD emission: what the analog code called.
type adState struct {
	externs map[string]bool                     // extern names called
	funcs   map[string]*elaborator.FunctionInfo // user functions called, by C name
}

var (
	reDouble = regexp.MustCompile(`\bdouble\b`)
	reIdent  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// emitJacobianPhase emits the Dual function overloads and phase_jacobian, or
// nothing when the deck cannot be differentiated. Sets g.hasJacobianPhase.
func (g *generator) emitJacobianPhase() {
	writes, ok := g.collectAnalogWrites()
	if !ok || len(writes) == 0 {
		return
	}

	// The sources an analog-written double drives. Without any, analog code
	// never reaches B and there is nothing to differentiate.
	type stamp struct{ call, handle, sig string }
	var stamps []stamp
	for _, phys := range g.prog.Physicals {
		if phys.CtrlSignal == "" {
			continue
		}
		sig := mangle(phys.CtrlSignal)
		if !writes[sig] || g.typeOf(sig).elem != CDouble {
			continue
		}
		switch strings.ToLower(phys.Type) {
		case "voltage_source":
			stamps = append(stamps, stamp{"ad_voltage_source", g.handle(handleVoltageSource, phys.Name), sig})
		case "current_source", "i":
			stamps = append(stamps, stamp{"ad_current_source", g.handle(handleCurrentSource, phys.Name), sig})
		}
	}
	if len(stamps) == 0 {
		return
	}

	// 1. The analog blocks, emitted as usual into a scratch region of g.sb.
	st := &adState{externs: map[string]bool{}, funcs: map[string]*elaborator.FunctionInfo{}}
	g.ad = st
	body := g.captureEmit(func() {
		g.push()
		for _, logic := range g.prog.Logic {
			if logic.Domain != token.ANALOG || logic.Clock != "" {
				continue
			}
			g.line("// %s", logic.Prefix)
			g.emitStmt(logic.Source, logic, false)
		}
		g.pop()
	})

	// 2. Every user function reachable from it, transitively.
	var fnNames []string
	var fnBodies []string
	done := map[string]bool{}
	for {
		var pending []string
		for name := range st.funcs {
			if !done[name] {
				pending = append(pending, name)
			}
		}
		if len(pending) == 0 {
			break
		}
		sort.Strings(pending)
		for _, name := range pending {
			done[name] = true
			fn := st.funcs[name]
			if !g.adFunctionOK(fn.Decl) {
				g.ad = nil
				return
			}
			if !g.adNeedsOverload(fn.Decl) {
				continue // no double parameter: the original serves both
			}
			fnNames = append(fnNames, name)
			fnBodies = append(fnBodies, g.captureEmit(func() { g.emitOneFunction(fn) }))
		}
	}
	g.ad = nil

	for name := range st.externs {
		if !adMathExterns[name] {
			return // an extern with no Dual overload: keep the numerical Jacobian
		}
	}

	// 3. Rewrite the captured C++ into its Dual form and emit it.
	g.raw(`// ── AUTOMATIC DIFFERENTIATION — Dual overloads of the functions analog code calls ─`)
	g.raw(`// (compiler/codegen/autodiff.go; hvr::Dual is in hover_sim.hpp)`)
	for i := range fnNames {
		fn := st.funcs[fnNames[i]]
		params := []string{"hvr::Sim &sim"}
		for _, p := range fn.Decl.Parameters {
			params = append(params, g.hoverTypeOf(p.Type).cParamDecl(p.Name))
		}
		decl := fmt.Sprintf("static %s %s(%s);", g.hoverTypeOf(fn.Decl.ReturnType).cReturnType(), fn.CName, strings.Join(params, ", "))
		g.raw(adRewrite(decl, nil))
	}
	g.raw("")
	for _, b := range fnBodies {
		g.sb.WriteString(adRewrite(b, nil))
	}

	g.raw(`// ── PHASE_JACOBIAN — the analog blocks in hvr::Dual: writes -dB/dx ─────────────`)
	g.raw(`static void phase_jacobian(hvr::Sim &sim) {`)
	g.push()
	g.line("(void)sim;")
	vars := make([]string, 0, len(writes))
	for v := range writes {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	for _, v := range vars {
		g.line("%s = %s;", adRewrite(g.typeOf(v).cVarDecl("ad_"+v), nil), v)
	}
	g.pop()
	g.sb.WriteString(adRewrite(body, writes))
	g.push()
	for _, s := range stamps {
		g.line("sim.%s(%s, ad_%s);", s.call, s.handle, s.sig)
	}
	g.pop()
	g.raw("}")
	g.raw("")
	g.hasJacobianPhase = true
}

// captureEmit runs emit and returns what it wrote, removing it from g.sb.
func (g *generator) captureEmit(emit func()) string {
	start := g.sb.Len()
	nErr := len(g.errors) // the first emission already reported anything wrong
	emit()
	g.errors = g.errors[:nErr]
	all := g.sb.String()
	g.sb.Reset()
	g.sb.WriteString(all[:start])
	return all[start:]
}

// adRewrite turns emitted double code into its Dual form: the type, the reads
// of the unknowns and the state slots, and — in the phase body — the
// analog-written globals into their Dual locals.
func adRewrite(code string, locals map[string]bool) string {
	code = reDouble.ReplaceAllString(code, "hvr::Dual")
	code = strings.NewReplacer(
		"sim.V(", "sim.V_ad(",
		"sim.I(", "sim.I_ad(",
		"sim.ddt(", "sim.ddt_ad(",
		"sim.idt(", "sim.idt_ad(",
	).Replace(code)
	if locals != nil {
		code = reIdent.ReplaceAllStringFunc(code, func(id string) string {
			if locals[id] {
				return "ad_" + id
			}
			return id
		})
	}
	return code
}

// collectAnalogWrites returns every global an analog block assigns. ok is
// false when one of them is something a Dual local cannot stand in for.
func (g *generator) collectAnalogWrites() (map[string]bool, bool) {
	writes := map[string]bool{}
	ok := true
	var walk func(s ast.Statement, logic elaborator.LogicObject)
	walk = func(s ast.Statement, logic elaborator.LogicObject) {
		if s == nil || !ok {
			return
		}
		switch n := s.(type) {
		case *ast.BlockStatement:
			if n == nil {
				return
			}
			for _, c := range n.Body {
				walk(c, logic)
			}
		case *ast.IfStatement:
			if n == nil {
				return
			}
			walk(n.Consequence, logic)
			for _, alt := range n.Alternatives {
				walk(alt.Body, logic)
			}
			walk(n.Alternative, logic)
		case *ast.WhileStatement:
			if n == nil {
				return
			}
			walk(n.Body, logic)
		case *ast.LocalDeclStatement:
			if n == nil || n.Type.IsWire() {
				return
			}
			for _, d := range n.Decls {
				writes[resolveWrite(d.Name, logic)] = true
			}
		case *ast.AssignmentStatement:
			if n == nil {
				return
			}
			id, isIdent := n.Left.(*ast.IdentifierExpression)
			if !isIdent {
				ok = false // an element or a field: no scalar Dual local for it
				return
			}
			writes[resolveWrite(id.Value, logic)] = true
		}
	}
	for _, logic := range g.prog.Logic {
		if logic.Domain == token.ANALOG && logic.Clock == "" {
			walk(logic.Source, logic)
		}
	}
	for v := range writes {
		ht := g.typeOf(v)
		if ht.isArray() || ht.isPointer() || ht.elem == CStruct {
			return nil, false
		}
	}
	return writes, ok
}

// adFunctionOK: a function the Dual copy can be made of — scalar parameters
// and return value only.
func (g *generator) adFunctionOK(decl *ast.FuncDeclStatement) bool {
	scalar := func(t ast.Type) bool {
		ht := g.hoverTypeOf(t)
		return !ht.isArray() && !ht.isPointer() && ht.elem != CStruct
	}
	for _, p := range decl.Parameters {
		if !scalar(p.Type) {
			return false
		}
	}
	return scalar(decl.ReturnType)
}

// adNeedsOverload: whether the function takes a double at all — one that
// does not would get a Dual copy with an identical signature.
func (g *generator) adNeedsOverload(decl *ast.FuncDeclStatement) bool {
	for _, p := range decl.Parameters {
		if g.hoverTypeOf(p.Type).elem == CDouble {
			return true
		}
	}
	return false
}
