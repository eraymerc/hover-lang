package codegen

import (
	"fmt"
	"sort"
	"strings"

	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
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
	g.line("std::vector<std::string> vm_signals = {%s};", quotedList(cfg.saveVMSignals))
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
		if decl := g.lookupFunctionDecl(callExpressionName(v.Function)); decl != nil && decl.IsExtern {
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

	g.pop()
	g.raw("}")
	g.raw("")
}

// mainBlock returns the main module's LogicObject — the one whose `()`
// logic args and `<>` static args become HVR_set_input_*/HVR_set_param_*.
// Falls back to the first logic block if none is prefixed "main" (should
// not happen for a well-formed entry file, but this keeps codegen from
// panicking on an unusual elaboration shape rather than silently emitting
// no setters).
func (g *generator) mainBlock() *elaborator.LogicObject {
	for i := range g.prog.Logic {
		if g.prog.Logic[i].Prefix == "main" {
			return &g.prog.Logic[i]
		}
	}
	if len(g.prog.Logic) > 0 {
		return &g.prog.Logic[0]
	}
	return nil
}

// emitInputSetters emits HVR_set_input_<name>(value) for every `()` logic
// arg of the main module that resolved to a real scalar C++ global (i.e.
// is present in collectAllVars() — this filters out physical/wire ports,
// which are topological and have no runtime storage of their own).
func (g *generator) emitInputSetters() {
	mb := g.mainBlock()
	if mb == nil {
		return
	}
	allVars := map[string]bool{}
	for _, v := range g.collectAllVars() {
		allVars[v] = true
	}
	for _, name := range sortedKeys(mb.Ports) {
		mangled := mangle(mb.Ports[name])
		if !allVars[mangled] {
			continue
		}
		ht := g.typeOf(mangled)
		if ht.isArray() {
			continue // array inputs need an explicit-length setter — not yet supported
		}
		g.raw(fmt.Sprintf("void HVR_set_input_%s(%s v) {", sanitizeIdent(name), ht.elem.String()))
		g.push()
		g.line("%s = v;", mangled)
		g.pop()
		g.raw("}")
		g.raw("")
	}
}

// emitParamSetters emits HVR_set_param_<name>(value) plus a backing global
// for every `<>` static arg of the main module.
//
// CAVEAT: `<>` static args are substituted as compile-time literals during
// elaboration today (see elaborator.evalStatic) — the whole point of `<>`
// vs `()` in Hover's module syntax. So while HVR_set_param_* writes this
// global correctly (and enforces the t==0 rule), nothing in the generated
// equations reads it yet: the literal is already baked in elsewhere in
// this file. Wiring that up needs the elaborator to emit a global
// reference for the main module's `<>` args instead of inlining the
// literal — that change is out of scope here and is the one deliberate
// stub in this feature.
func (g *generator) emitParamSetters() {
	mb := g.mainBlock()
	if mb == nil {
		return
	}
	for _, name := range sortedKeys(mb.Params) {
		global := "HVR_param_" + sanitizeIdent(name)
		g.raw(fmt.Sprintf("// CAVEAT: %s is not yet wired into the equations — see the", global))
		g.raw("// emitParamSetters comment in hovercraft_emit.go.")
		g.raw(fmt.Sprintf("double %s = %s;", global, formatTypedLiteral(mb.Params[name], CDouble)))
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
// HVR_set_param_<name>.
func sanitizeIdent(s string) string {
	return strings.ReplaceAll(s, ".", "_")
}
