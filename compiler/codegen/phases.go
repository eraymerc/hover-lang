package codegen

import (
	"fmt"
	"hover/compiler/token"
	"sort"
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
//
// clock selects which digital logic lands here: "" for the unclocked logic
// that runs every step, or a clock's name for the logic bound to it. It is
// "" for the structural and analog phases, whose logic is never clocked.
func (g *generator) emitPhaseByDomain(domain token.Type, fnName string, clock string) {
	g.raw(fmt.Sprintf("// ── %s ─────────────────────────────────────────────────────────────────────", strings.ToUpper(fnName)))
	g.raw(fmt.Sprintf("static void %s(hvr::Sim &sim) {", fnName))
	g.push()
	g.line("(void)sim;")
	if domain != token.ANALOG && clock == "" {
		g.raw(bpLogicPlaceholder(fnName)) // breakpoints.go: finishBreakpoints
	}
	if domain == token.ANALOG && len(g.nrSlots) > 0 {
		g.line("sim.nr_begin();  // nr_prev() of analog variables (runtime/vm/nr_locals.hpp)")
	}

	// All variables are file-scope globals — no C blocks needed.
	// Emit statements flat; use comments to mark module boundaries.
	for _, logic := range g.prog.Logic {
		if logic.Domain != domain || logic.Clock != clock {
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
	g.emitPhaseByDomain(token.MODULE, "phase_structural", "")
}

// emitClockPhases emits one phase function per `.clock` domain, holding the
// digital modules bound to it with `@name`. The runtime calls each once per
// tick (vm/clock.cpp); emitSimSetup registers them with sim.add_clock.
func (g *generator) emitClockPhases() {
	for _, c := range g.clocks {
		g.emitPhaseByDomain(token.DIGITAL, c.phaseFunc(), c.name)
	}
}

func (g *generator) emitPhaseDigital() {
	g.emitPhaseByDomain(token.DIGITAL, "phase_digital", "")
}

// emitPhaseAnalog is in nrlocals.go.

// branchCurrentColumn is the vm->values key (and hence CSV column header) for
// a .save()d element current. Element and net names are disjoint by
// construction — the elaborator rejects a collision — but the "I(...)" wrapper
// keeps the CSV self-describing: a reader can tell a current column from a
// voltage column without having the netlist in front of them.
func branchCurrentColumn(element string) string { return "I(" + element + ")" }

// emitPhaseLog emits phase_log, which publishes every .save()d quantity that
// isn't already in the solution vector into vm->values, so the logger can read
// them generically (logger.cpp only knows how to read a string-keyed map, not
// C globals by pointer).
//
// Two kinds land here:
//   - VM signals — logic variables, copied from their C globals.
//   - Branch currents — read back out of the solved system with api_I. This is
//     why .save(main.vsense) needs no logger change at all: every solver calls
//     logger_log_step -> phase_log -> logger_log_signals in that order, after
//     api_update_solution has published this timestep's solution, so api_I here
//     returns exactly the value logger_log_step just recorded voltages from.
//
// A VM signal that doesn't correspond to an actual logic-block variable is
// skipped with an explanatory comment rather than emitted as a broken
// reference to a nonexistent C global.
func (g *generator) emitPhaseLog(vmSignals, branchCurrents []string) {
	allVars := g.collectAllVars()
	known := make(map[string]bool, len(allVars))
	for _, v := range allVars {
		known[v] = true
	}

	g.raw(`// ── PHASE LOG — publish saved values into vm->values for logger ──────────────`)
	g.raw(`static void phase_log(hvr::Sim &sim) {`)
	g.push()
	g.line("(void)sim;")
	for _, elem := range branchCurrents {
		g.line(`sim.set_value(%s, sim.I(%s));`,
			cStr(branchCurrentColumn(elem)), g.handle(handleBranch, elem))
	}
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
		ht := g.typeOf(mangled)
		if ht.isArray() || ht.isPointer() || ht.elem == CStruct {
			g.line(`// skipped: %s is an array/pointer/struct (not scalar-loggable)`, sig)
			continue
		}
		g.line(`sim.set_value(%s, %s);`, cStr(sig), emitCast(mangled, ht.elem, CDouble))
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)
}

// emitStateVarSnapshot emits save_state_vars and restore_state_vars,
// which copy every Hover `state`-declared variable's value into/from
// vm->state_vals, by an index fixed here.
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
	stateNames := g.collectStateInits()
	dottedNames := g.collectStateVarDottedNames()

	// Sorted, because both loops below iterate this set and emit one line per
	// entry. Ranging the map directly meant two builds of identical source
	// produced sim.cpp files whose save/restore bodies had the same statements
	// in different orders — observable by running examples/DCMotor twice.
	//
	// The emitted code is correct either way (these are independent
	// assignments), so this is purely about reproducibility — which is worth
	// having on its own, and doubly so given that hpm identifies every package
	// by the hash of its contents.
	mangledNames := make([]string, 0, len(stateNames))
	for mangled := range stateNames {
		mangledNames = append(mangledNames, mangled)
	}
	sort.Strings(mangledNames)

	// Scalar state variables get consecutive indices into vm->state_vals —
	// the same order in both functions. (Until 0.9 they went into vm->values
	// by dotted name: a string build and a hash per variable on every
	// snapshot, and the solvers and .zcd probes take several per step.)
	scalars := make([]string, 0, len(mangledNames))
	for _, mangled := range mangledNames {
		ht := g.typeOf(mangled)
		if ht.isArray() || ht.isPointer() || ht.elem == CStruct {
			continue
		}
		scalars = append(scalars, mangled)
	}

	// save_state_vars: state global -> vm->state_vals[k]
	g.raw(`// ── STATE SNAPSHOT — save 'state' variables into vm->state_vals ─────────────`)
	g.raw(`// Used by vm_save_state (snapshot.cpp) so ZCD probes and rejected solver`)
	g.raw(`// steps can actually be rolled back — see emitStateVarSnapshot in codegen.`)
	g.raw(`static void save_state_vars(hvr::Sim &sim) {`)
	g.push()
	g.line("(void)sim;")
	for k, mangled := range scalars {
		ht := g.typeOf(mangled)
		g.line(`sim.set_state_var(%d, %s);  // %s`, k, emitCast(mangled, ht.elem, CDouble), dottedNames[mangled])
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)

	// restore_state_vars: vm->state_vals[k] -> state global, cast back
	//
	// Struct-typed state (like array/pointer state) is excluded, same as
	// above: vm->state_vals holds doubles, which have no way to hold an
	// aggregate. A struct-typed `state` variable therefore won't be
	// rolled back by a ZCD probe or a rejected solver step the way a
	// scalar `state` variable is — a known limitation of this first
	// version of struct support, not a silently-wrong cast.
	g.raw(`// ── STATE SNAPSHOT — restore 'state' variables from vm->state_vals ──────────`)
	g.raw(`static void restore_state_vars(hvr::Sim &sim) {`)
	g.push()
	g.line("(void)sim;")
	for k, mangled := range scalars {
		ht := g.typeOf(mangled)
		g.line(`%s = %s;`, mangled, emitCast(fmt.Sprintf(`sim.state_var(%d)`, k), CDouble, ht.elem))
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
	g.raw(`static void phase_b(hvr::Sim &sim) {`)
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
		sigType := g.typeOf(sig).elem
		castSig := emitCast(sig, sigType, CDouble)
		switch strings.ToLower(phys.Type) {
		case "voltage_source":
			g.line(`sim.set_voltage_source(%s, %s);`, g.handle(handleVoltageSource, name), castSig)
		case "current_source", "i":
			g.line(`sim.set_current_source(%s, %s);`, g.handle(handleCurrentSource, name), castSig)
		}
	}

	g.pop()
	g.raw("}")
	g.raw("")
}
