package codegen

import (
	"fmt"
	"sort"
	"strings"

	ast "hover/compiler/ast"
)

// ─────────────────────────────────────────────────────────────────────────────
// --hovercraft LIBRARY MODE
//
// Instead of a one-shot main() that builds System/Solver/API/VM as locals,
// runs to end_time, and exits (see emitMain's standalone branch in
// main_emit.go — left completely untouched), library mode wires those same
// objects up as file-scope statics and exposes them through the HVR_* C ABI
// declared in runtime/hovercraft.h. The generic log-retrieval / lifecycle
// glue (hvr_rt_*) lives in runtime/vm/hvr_runtime.cpp, against Logger's
// public fields; vm_boot/vm_run_until (the incremental split of the
// previously-monolithic vm_run) live in runtime/vm/vm.cpp itself.
// ─────────────────────────────────────────────────────────────────────────────

// emitHvrParamGlobals declares the backing globals for main's `<>` static
// args — double HVR_param_<name> = <initial literal>; — ahead of every
// other function in the file. It must run before emitFunctions/the phase
// emitters (see emit() in generator.go): resolveIdent (names.go) now emits
// a reference to this exact global wherever a library-mode equation reads
// one of main's own <> args, so the global has to already be declared at
// that point in the translation unit. emitParamSetters (below) later emits
// the HVR_set_param_<name> setter against the same global — it does not
// redeclare it.
func (g *generator) emitHvrParamGlobals() {
	params := g.prog.MainParams
	if len(params) == 0 {
		return
	}
	g.raw(`// ── HOVERCRAFT <> PARAM GLOBALS ─────────────────────────────────────────────`)
	g.raw("// Backing storage for main's <> args, settable at runtime via")
	g.raw("// HVR_set_param_<name>() (emitParamSetters, below) and read by every")
	g.raw("// equation that used to have the literal inlined at elaboration time.")
	for _, name := range sortedKeys(params) {
		g.raw(fmt.Sprintf("double HVR_param_%s = %s;", sanitizeIdent(name), formatTypedLiteral(params[name], CDouble)))
	}
	g.raw("")
}

// emitLibraryMain is emitMain's --hovercraft branch.
func (g *generator) emitLibraryMain(cfg simConfig, strategyStruct string) error {
	g.raw(`#include "hvr_runtime.hpp"`)
	g.raw(`#include "hovercraft.h"`)
	g.raw(`#include <cstring>`)
	g.raw("")

	g.raw(`// ── HOVERCRAFT RUNTIME WIRING ───────────────────────────────────────────────`)
	g.raw("// File-scope statics (not main()-locals) so every HVR_* call below reaches")
	g.raw("// the one live instance across separate calls from the host program.")
	g.raw("static System sys;")
	g.raw("static Solver solver;")
	g.raw("static API    api;")
	g.raw("static VM     vm;")
	g.raw(fmt.Sprintf("static %s strategy;", strategyStruct))
	g.raw("static bool   hvr_wired   = false;")
	g.raw("static bool   hvr_started = false;")
	g.raw("")

	g.emitHvrApplyDeferredStateInits()

	g.raw("// hvr_boot() wires System/Solver/API/VM together exactly once. It")
	g.raw("// deliberately does NOT run the numerical OP/first-step init — that is")
	g.raw("// vm_boot()'s job (runtime/vm/vm.cpp), triggered lazily by")
	g.raw("// hvr_ensure_started() below, so a host program gets the chance to call")
	g.raw("// HVR_set_input_*/HVR_set_param_* before the first solve happens.")
	g.raw("static void hvr_boot(void) {")
	g.push()
	g.line("if (hvr_wired) return;")
	g.line("system_init_empty(&sys, %s);", cfg.tStep)
	g.line("register_netlist(&sys);")
	g.line("system_finalize(&sys);")
	g.line("stamp_netlist(&sys);")
	g.raw("")
	g.line("solver_init(&solver, &sys);")
	g.line("api_init(&api, &sys, &solver);")
	g.raw("")
	g.line("vm.api        = &api;")
	g.line("vm.solver     = &solver;")
	g.line("vm.time       = 0.0;")
	g.line("vm.time_step  = %s;", cfg.tStep)
	g.line("vm.end_time   = %s;", cfg.tEnd)
	g.line("vm.zcd_enabled = %d;", boolToInt(cfg.zcdEnabled))
	g.line("vm.op_enabled  = %d;", boolToInt(cfg.opEnabled))
	g.line("vm.jacobian_scratch = Eigen::MatrixXd::Zero(sys.size, sys.size);")
	g.line("vm.base_b_scratch   = Eigen::VectorXd::Zero(sys.size);")
	g.raw("")
	g.line("vm.phase_structural = phase_structural;")
	g.line("vm.phase_digital    = phase_digital;")
	g.line("vm.phase_analog     = phase_analog;")
	g.line("vm.phase_b          = phase_b;")
	g.line("vm.phase_log        = phase_log;")
	g.line("vm.save_state_vars    = save_state_vars;")
	g.line("vm.restore_state_vars = restore_state_vars;")
	g.raw("")
	g.line("std::vector<std::string> mna_nodes = {%s};", quotedList(cfg.saveMNANodes))
	g.line("std::vector<std::string> vm_signals = {%s};", quotedList(cfg.loggerSignalList()))
	g.line("logger_init(&vm.logger, mna_nodes, vm_signals);")
	g.raw("")
	g.emitSolverTuningOverrides(strategyStruct, cfg)
	g.line("vm.strategy = &strategy;")
	g.raw("")
	g.line("hvr_apply_deferred_state_inits();")
	g.line("hvr_wired = true;")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("// hvr_ensure_started() performs the numerical boot (vm_boot) the first")
	g.raw("// time any HVR_* call actually needs simulation state to exist.")
	g.raw("static void hvr_ensure_started(void) {")
	g.push()
	g.line("hvr_boot();")
	g.line("if (hvr_started) return;")
	g.line("vm_boot(&vm);")
	g.line("hvr_started = true;")
	g.pop()
	g.raw("}")
	g.raw("")

	g.emitHvrResetState()
	g.emitLibraryAPI(cfg)

	return nil
}

func (g *generator) isConstFoldableStateInit(si stateInit) bool {
	switch v := si.value.(type) {
	case nil:
		return true
	case *ast.NumberExpression:
		return true
	case *ast.CallExpression:
		if decl := g.lookupFunctionDecl(callExpressionName(v.Function), si.logic); decl != nil && decl.IsExtern {
			return true
		}
		return false
	default:
		return false
	}
}

func (g *generator) emitHvrApplyDeferredStateInits() {
	stateInits := g.collectStateInits()
	pending := false
	for _, name := range g.collectAllVars() {
		si, isState := stateInits[name]
		if !isState || g.typeOf(name).isArray() || g.isConstFoldableStateInit(si) {
			continue
		}
		if !pending {
			g.raw("static void hvr_apply_deferred_state_inits(void) {")
			g.push()
			pending = true
		}
		ht := g.typeOf(name)
		code, ct := g.emitExpr(si.value, si.logic)
		g.line("%s = %s;", name, castToHover(code, ct, ht))
	}
	if pending {
		g.pop()
		g.raw("}")
	} else {
		g.raw("static void hvr_apply_deferred_state_inits(void) {}")
	}
	g.raw("")
}

// emitHvrResetState emits hvr_reset_state(), which resets every state
// variable back to its original initializer expression — the exact same
// initializer emitStateVars used for the static's own initial value, so a
// reset is guaranteed to match what a fresh process would have started
// with. Scalars reassign directly; arrays go through a decltype+memcpy
// since C++ has no array assignment operator.
func (g *generator) emitHvrResetState() {
	g.raw("// hvr_reset_state() restores every state variable to the value it had")
	g.raw("// when the library was first loaded — used by HVR_reset_sim().")
	g.raw("static void hvr_reset_state(void) {")
	g.push()
	stateInits := g.collectStateInits()
	for _, name := range g.collectAllVars() {
		si, isState := stateInits[name]
		if !isState {
			continue
		}
		ht := g.typeOf(name)
		if ht.isArray() {
			g.line("{ static const decltype(%s) hvr_init = %s; memcpy(%s, hvr_init, sizeof(%s)); }",
				name, g.formatStateInitializer(ht, si), name, name)
		} else {
			g.line("%s = %s;", name, g.formatStateInitializer(ht, si))
		}
	}
	g.pop()
	g.raw("}")
	g.raw("")
}

// emitLibraryAPI emits the extern "C" HVR_* function definitions — the
// public surface declared in runtime/hovercraft.h — plus the per-signal
// setters (emitInputSetters/emitParamSetters below).
func (g *generator) emitLibraryAPI(cfg simConfig) {
	g.raw(`// ── HOVERCRAFT C ABI ─────────────────────────────────────────────────────────`)
	g.raw(`extern "C" {`)
	g.push()

	g.raw("int HVR_reset_sim(void) {")
	g.push()
	g.line("hvr_ensure_started();")
	g.line("hvr_reset_state();")
	g.line("hvr_apply_deferred_state_inits(); // re-sample current inputs, same as boot did")
	g.line("vm.time = 0.0;")
	g.line("vm_boot(&vm); // re-run OP / first-step init against the reset state")
	g.line("return HVR_OK;")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("void HVR_reset_log(void) {")
	g.push()
	g.line("hvr_rt_reset_log(&vm);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("int HVR_save_log(const char *filename) {")
	g.push()
	g.line("return hvr_rt_save_log_csv(&vm, filename);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("int HVR_step(long n) {")
	g.push()
	g.line("if (n < 0) return HVR_ERR_UNKNOWN;")
	g.line("hvr_ensure_started();")
	g.line("hvr_rt_mark_batch(&vm);")
	g.line("vm_run_until(&vm, vm.time + (double)n * vm.time_step);")
	g.line("return HVR_OK;")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("int HVR_run(double duration_seconds) {")
	g.push()
	g.line("if (duration_seconds < 0.0) return HVR_ERR_UNKNOWN;")
	g.line("hvr_ensure_started();")
	g.line("hvr_rt_mark_batch(&vm);")
	g.line("vm_run_until(&vm, vm.time + duration_seconds);")
	g.line("return HVR_OK;")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("HVRLogResult HVR_get_log(void) {")
	g.push()
	g.line("hvr_ensure_started();")
	g.line("return hvr_rt_query_all(&vm);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("HVRLogResult HVR_get_log_range(double t_start, double t_end) {")
	g.push()
	g.line("hvr_ensure_started();")
	g.line("return hvr_rt_query_range(&vm, t_start, t_end);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("HVRLogResult HVR_get_log_latest(void) {")
	g.push()
	g.line("hvr_ensure_started();")
	g.line("return hvr_rt_query_latest(&vm);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("HVRLogResult HVR_get_last_step(void) {")
	g.push()
	g.line("hvr_ensure_started();")
	g.line("return hvr_rt_query_last_step(&vm);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("void HVR_clear_log_before(double t) {")
	g.push()
	g.line("hvr_rt_clear_before(&vm, t);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.raw("void HVR_free_log_result(HVRLogResult *r) {")
	g.push()
	g.line("hvr_rt_free_result(r);")
	g.pop()
	g.raw("}")
	g.raw("")

	g.emitInputSetters()
	g.emitParamSetters()
	g.emitOutputGetters(cfg)

	g.pop()
	g.raw("}")
	g.raw("")
}

// hvrOutput is one readable quantity: the column name it carries in the
// log (so HVR_get_output's string lookup and an HVRLogResult column agree
// exactly) plus the C++ expression that reads its CURRENT value.
type hvrOutput struct {
	column string
	expr   string
}

// hvrOutputs enumerates every .save()d quantity as a directly-readable
// expression, in the same order logger_init lays the columns out (MNA
// nodes, then VM signals, then branch currents). The three kinds are read
// three different ways — node voltages come out of the solved system,
// branch currents out of the element's branch row, and logic signals
// straight off their C++ global — which is exactly the mapping phase_log
// performs each timestep; reading them here rather than out of
// vm->values means a getter is current even between logged steps.
func (g *generator) hvrOutputs(cfg simConfig) []hvrOutput {
	known := map[string]bool{}
	for _, v := range g.collectAllVars() {
		known[v] = true
	}

	var outs []hvrOutput
	for _, node := range cfg.saveMNANodes {
		outs = append(outs, hvrOutput{node, fmt.Sprintf("api_V(&api, %s)", cStr(node))})
	}
	for _, sig := range cfg.saveVMSignals {
		mangled := mangle(sig)
		ht := g.typeOf(mangled)
		// Same two exclusions phase_log applies: a name that isn't a real
		// logic variable has no global to read, and an array/pointer has no
		// single scalar value to return.
		if !known[mangled] || ht.isArray() || ht.isPointer() {
			continue
		}
		outs = append(outs, hvrOutput{sig, emitCast(mangled, ht.elem, CDouble)})
	}
	for _, elem := range cfg.saveBranchCurrents {
		outs = append(outs, hvrOutput{branchCurrentColumn(elem), fmt.Sprintf("api_I(&api, %s)", cStr(elem))})
	}
	return outs
}

// emitOutputGetters emits the per-signal readback API: a zero-overhead
// HVR_get_output_<name>() per .save()d column, plus the name-keyed
// HVR_get_output() declared in runtime/hovercraft.h.
//
// This complements the HVR_get_log* family rather than replacing it. A log
// query allocates an HVRLogResult, copies out every row, and has to be
// freed; a host polling one signal per step (a control loop reading a
// single node voltage, say) pays all of that to look at one number. These
// getters read the live value directly with no allocation.
//
// Both forms are generated: the per-signal functions are the fast path but
// their names depend on the .hvr source, so they can only be declared in
// the generated file. HVR_get_output(name, out) is name-keyed and so can
// live in the fixed public header — the only option for a host that
// discovers its columns at runtime (e.g. from HVRLogResult::names).
func (g *generator) emitOutputGetters(cfg simConfig) {
	outs := g.hvrOutputs(cfg)

	emitted := map[string]bool{}
	for _, o := range outs {
		fn := "HVR_get_output_" + sanitizeIdent(o.column)
		// Two distinct columns can only collide here if sanitizeIdent maps
		// them onto one identifier. Skipping the later one keeps the file
		// compiling (a duplicate definition is a hard C++ error) and the
		// name-keyed HVR_get_output below still reaches both.
		if emitted[fn] {
			g.raw(fmt.Sprintf("// skipped: %s collides with an already-emitted getter name —", fn))
			g.raw(fmt.Sprintf("// use HVR_get_output(%s, &out) to read this column.", cStr(o.column)))
			g.raw("")
			continue
		}
		emitted[fn] = true
		g.raw(fmt.Sprintf("double %s(void) {", fn))
		g.push()
		g.line("hvr_ensure_started();")
		g.line("return %s;", o.expr)
		g.pop()
		g.raw("}")
		g.raw("")
	}

	g.raw("// Name-keyed readback — see runtime/hovercraft.h. Returns")
	g.raw("// HVR_ERR_UNKNOWN (leaving *out untouched) for a name that isn't a")
	g.raw("// .save()d column, so a caller can distinguish 'no such signal' from")
	g.raw("// a signal that is legitimately 0.")
	g.raw("int HVR_get_output(const char *name, double *out) {")
	g.push()
	g.line("if (name == nullptr || out == nullptr) return HVR_ERR_UNKNOWN;")
	g.line("hvr_ensure_started();")
	for _, o := range outs {
		g.line("if (strcmp(name, %s) == 0) { *out = %s; return HVR_OK; }", cStr(o.column), o.expr)
	}
	g.line("return HVR_ERR_UNKNOWN;")
	g.pop()
	g.raw("}")
	g.raw("")
}

// emitInputSetters emits HVR_set_input_<name>(...) for every `()` logic
// arg of the main module that resolved to a real scalar or array C++
// global (i.e. is present in collectAllVars() — this filters out
// physical/wire ports, which are topological and have no runtime storage
// of their own).
//
// Scalar inputs get a plain value setter. Array inputs get an
// explicit-length setter — HVR_set_input_<name>(const T *values, long n)
// — since C++ has no array-assignment operator and the host program's
// buffer length isn't otherwise knowable at the ABI boundary: n is
// clamped to the array's declared capacity, and a caller passing fewer
// than the full capacity only overwrites that many leading elements.
func (g *generator) emitInputSetters() {
	allVars := map[string]bool{}
	for _, v := range g.collectAllVars() {
		allVars[v] = true
	}
	for _, name := range sortedKeys(g.prog.MainPorts) {
		mangled := mangle(g.prog.MainPorts[name])
		if !allVars[mangled] {
			continue
		}
		ht := g.typeOf(mangled)
		if ht.isArray() {
			g.raw(fmt.Sprintf("void HVR_set_input_%s(const %s *values, long n) {", sanitizeIdent(name), ht.elem.String()))
			g.push()
			g.line("if (n <= 0) return;")
			g.line("long cap = %d;", ht.elemCount())
			g.line("long count = n < cap ? n : cap;")
			g.line("memcpy(%s, values, (size_t)count * sizeof(%s));", mangled, ht.elem.String())
			g.pop()
			g.raw("}")
			g.raw("")
			continue
		}
		g.raw(fmt.Sprintf("void HVR_set_input_%s(%s v) {", sanitizeIdent(name), ht.elem.String()))
		g.push()
		g.line("%s = v;", mangled)
		g.pop()
		g.raw("}")
		g.raw("")
	}
}

// emitParamSetters emits HVR_set_param_<name>(value) for every `<>` static
// arg of the main module, writing the backing global emitHvrParamGlobals
// declared up-front — the same global every library-mode equation reads
// via resolveIdent (names.go), so calling this actually reaches the
// simulation. Only allowed before the sim has advanced (t==0): main's <>
// args are still baked in as of-t=0 constants for every OTHER module's
// static args (submodule instantiations resolve those to plain floats at
// elaboration time, same as ever), so changing one mid-run would leave
// stamped element values inconsistent with the new setting.
func (g *generator) emitParamSetters() {
	for _, name := range sortedKeys(g.prog.MainParams) {
		global := "HVR_param_" + sanitizeIdent(name)
		g.raw(fmt.Sprintf("int HVR_set_param_%s(double v) {", sanitizeIdent(name)))
		g.push()
		g.line("if (vm.time > 0.0) return HVR_ERR_TIME;")
		g.line("%s = v;", global)
		g.line("return HVR_OK;")
		g.pop()
		g.raw("}")
		g.raw("")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SMALL HELPERS
// ─────────────────────────────────────────────────────────────────────────────

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sanitizeIdent turns a dotted or otherwise non-identifier-safe Hover name
// into a valid trailing C identifier segment for HVR_set_input_<name> /
// HVR_set_param_<name> / HVR_get_output_<name>.
//
// Every character outside [A-Za-z0-9_] becomes '_', not just '.': a
// branch-current column is spelled "I(main.vsense)", whose parentheses
// would produce a syntactically invalid function name if only dots were
// replaced. Trailing underscores are trimmed so that column reads
// HVR_get_output_I_main_vsense rather than ..._vsense_.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.TrimRight(b.String(), "_")
}
