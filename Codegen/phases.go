package codegen

import (
	"fmt"
	"hover/Interpreter/token"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// PHASE EMITTERS
// Each Hover domain (module/digital/analog) becomes its own C function,
// called in fixed order by the generated main loop via VM's phase function
// pointers. See vm.hpp/vm.cpp for how phase_structural/phase_digital/
// phase_analog/phase_b/phase_log are wired up and invoked each timestep.
// ─────────────────────────────────────────────────────────────────────────────

// emitPhaseByDomain emits one phase function (phase_structural, phase_digital,
// or phase_analog) by walking every LogicObject whose Domain matches and
// emitting its statement. All variables involved are file-scope globals
// (see collect.go/emitStateVars), so no C block scoping is needed here —
// statements from different LogicObjects in the same domain can simply be
// emitted flat, one after another.
func (g *generator) emitPhaseByDomain(domain token.Type, fnName string) {
	g.raw(fmt.Sprintf("// ── %s ─────────────────────────────────────────────────────────────────────", strings.ToUpper(fnName)))
	g.raw(fmt.Sprintf("static void %s(VM *vm) {", fnName))
	g.push()
	g.line("(void)(vm);")

	// All variables are file-scope globals — no C blocks needed.
	// Emit statements flat; use comments to mark module boundaries.
	for _, logic := range g.prog.Logic {
		if logic.Domain != domain {
			continue
		}
		g.line("// %s", logic.Prefix)
		g.emitStmt(logic.Source, logic, false)
	}

	g.pop()
	g.raw("}")
	g.raw("")
}

func (g *generator) emitPhaseStructural() {
	g.emitPhaseByDomain(token.MODULE, "phase_structural")
}

func (g *generator) emitPhaseDigital() {
	g.emitPhaseByDomain(token.DIGITAL, "phase_digital")
}

func (g *generator) emitPhaseAnalog() {
	g.emitPhaseByDomain(token.ANALOG, "phase_analog")
}

// emitPhaseLog emits phase_log, which copies the requested .save() VM
// signals from their C globals into vm->values so the logger can read them
// generically (logger.cpp only knows how to read a string-keyed map, not
// C globals by pointer).
//
// Signals that don't correspond to an actual logic-block variable (for
// example a raw MNA branch current referenced via I(...) in a .save())
// are skipped with an explanatory comment rather than emitted as a broken
// reference to a nonexistent C global.
func (g *generator) emitPhaseLog(vmSignals []string) {
	allVars := g.collectAllVars()
	known := make(map[string]bool, len(allVars))
	for _, v := range allVars {
		known[v] = true
	}

	g.raw(`// ── PHASE LOG — copy C globals into vm->values for logger ────────────────────`)
	g.raw(`static void phase_log(VM *vm) {`)
	g.push()
	for _, sig := range vmSignals {
		mangled := mangle(sig)
		if !known[mangled] {
			// Signal not a logic variable — skip (may be a branch current or MNA node)
			g.line(`// skipped: %s not a logic variable`, sig)
			continue
		}
		// vm->values is a map<string,double> — explicit cast even though
		// int64_t/uint64_t/float all convert to double implicitly without
		// warning, per the explicit-cast-at-every-type-boundary philosophy
		// applied consistently throughout this type system.
		sigType := g.typeOf(mangled)
		castExpr := emitCast(mangled, sigType, CDouble)
		g.line(`vm->values[%s] = %s;`, cStr(sig), castExpr)
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)
}

// emitStateVarSnapshot emits save_state_vars and restore_state_vars,
// which copy every Hover `state`-declared variable's value into/from
// vm->values, under the same dotted Hover name phase_log uses.
//
// These exist solely so vm_save_state/vm_restore_state (snapshot.cpp) can
// actually roll back real program state during a ZCD probe or solver step
// rejection. Without them, a `state int counter` that gets advanced during
// a probe-ahead check (see vm_check_zero_crossing in zcd.cpp) has no way
// to be rolled back — the probe's "restore" only ever touched vm->values
// and the MNA solution vector, never the C++ static globals that actually
// hold state. This was the root cause of a `state unsigned int` counter
// advancing by 2 per logged timestep instead of 1 whenever .zcd was
// enabled: the probe ran phase_digital once (advancing the counter), and
// the restore step had nothing that could undo it.
//
// Only `state` variables are included — plain (non-state) locals are
// recomputed from scratch by their own LogicObject's statement every time
// a phase runs, so there is nothing meaningful to save/restore for them;
// whatever value a probe leaves in a plain local is overwritten on the
// very next real phase call regardless.
func (g *generator) emitStateVarSnapshot() {
	stateVars := g.collectStateVars()
	dottedNames := g.collectStateVarDottedNames()

	// save_state_vars: state global -> vm->values[dottedName]
	g.raw(`// ── STATE SNAPSHOT — save 'state' variables into vm->values ─────────────────`)
	g.raw(`// Used by vm_save_state (snapshot.cpp) so ZCD probes and rejected solver`)
	g.raw(`// steps can actually be rolled back — see emitStateVarSnapshot in codegen.`)
	g.raw(`static void save_state_vars(VM *vm) {`)
	g.push()
	g.line("(void)(vm);")
	for mangled := range stateVars {
		dotted := dottedNames[mangled]
		ctype := g.typeOf(mangled)
		castExpr := emitCast(mangled, ctype, CDouble)
		g.line(`vm->values[%s] = %s;`, cStr(dotted), castExpr)
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)

	// restore_state_vars: vm->values[dottedName] -> state global, cast back
	g.raw(`// ── STATE SNAPSHOT — restore 'state' variables from vm->values ──────────────`)
	g.raw(`static void restore_state_vars(VM *vm) {`)
	g.push()
	g.line("(void)(vm);")
	for mangled := range stateVars {
		dotted := dottedNames[mangled]
		ctype := g.typeOf(mangled)
		castExpr := emitCast(fmt.Sprintf(`vm->values[%s]`, cStr(dotted)), CDouble, ctype)
		g.line(`%s = %s;`, mangled, castExpr)
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)
}

// emitPhaseB emits phase_b, which stamps every driven (non-fixed) physical
// primitive's controlling signal into the MNA system each timestep —
// voltage_source and current_source primitives that have a logic-port
// input wired to a Hover signal rather than a compile-time constant.
func (g *generator) emitPhaseB() {
	g.raw(`// ── PHASE B — STAMP DRIVEN SOURCES ──────────────────────────────────────────`)
	g.raw(`static void phase_b(VM *vm) {`)
	g.push()

	for _, phys := range g.prog.Physicals {
		if phys.CtrlSignal == "" {
			continue
		}
		sig := mangle(phys.CtrlSignal)
		name := phys.Name
		// api_set_voltage_source / api_set_current_source both take a
		// plain double — explicit cast even though int64_t/uint64_t/float
		// convert implicitly without warning, per the explicit-cast
		// philosophy applied consistently throughout this type system.
		sigType := g.typeOf(sig)
		castSig := emitCast(sig, sigType, CDouble)
		switch strings.ToLower(phys.Type) {
		case "voltage_source":
			g.line(`api_set_voltage_source(vm->api, %s, %s);`, cStr(name), castSig)
		case "current_source", "i":
			g.line(`api_set_current_source(vm->api, %s, %s);`, cStr(name), castSig)
		}
	}

	g.pop()
	g.raw("}")
	g.raw("")
}
