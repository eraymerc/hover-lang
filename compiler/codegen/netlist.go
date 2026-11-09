package codegen

import (
	"fmt"
	"hover/compiler/elaborator"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NETLIST CONSTRUCTION
//
// Two-pass build, mirroring the runtime's System lifecycle:
//
//  1. register_netlist — calls sys->register_node/register_branch for every
//     physical primitive's terminals. Must happen before system_finalize()
//     sizes the Eigen matrices, since the final node/branch count isn't
//     known until every primitive has been walked once.
//  2. stamp_netlist — calls sys->resolve_node/resolve_branch (now valid,
//     post-finalize) and the actual stamp_* functions to fill in G/C/B.
//
// See system.hpp's system_init_empty/system_finalize and emitMain's call
// order in main_emit.go for the other half of this contract.
// ─────────────────────────────────────────────────────────────────────────────

func (g *generator) emitBuildNetlist() {
	// ── Pass 1: register all nodes and branches (no stamping yet) ────────────────
	g.raw(`// ── NETLIST PASS 1: register nodes & branches ────────────────────────────────`)
	g.raw(`static void register_netlist(hvr::Sim &sim) {`)
	g.push()

	for _, phys := range g.prog.Physicals {
		g.emitRegisterCall(phys)
	}

	g.pop()
	g.raw("}")
	g.raw("")

	// ── Pass 2: stamp values into the now-correctly-sized matrices ────────────────
	g.raw(`// ── NETLIST PASS 2: stamp values ─────────────────────────────────────────────`)
	g.raw(`static void stamp_netlist(hvr::Sim &sim) {`)
	g.push()

	for _, phys := range g.prog.Physicals {
		g.emitStampCall(phys)
	}

	g.pop()
	g.raw("}")
	g.raw("")
}

// emitRegisterCall emits the pass-1 registration calls for one physical
// primitive: every node it touches, plus a branch if its type needs one
// (inductors and voltage-defined sources require an extra MNA row for
// their branch current; resistors/capacitors/current sources do not).
func (g *generator) emitRegisterCall(phys elaborator.PhysicalObject) {
	// Register all nodes this primitive touches
	for _, n := range phys.Nodes {
		cleanN := n // elaborator names are structural now — nothing to scrub
		if cleanN != "gnd" && cleanN != "0" && cleanN != "" {
			g.line("sim.register_node(%s);", cStr(cleanN))
		}
	}
	// Register branch if this primitive needs one
	if branchBearingType(phys.Type) {
		g.line("sim.register_branch(%s);", cStr(phys.Name))
	}
}

// branchBearingType reports whether a primitive gets its own MNA branch row —
// an extra unknown for its current. Inductors and voltage-defined sources need
// one; resistors, capacitors and current sources do not, since their current
// is derived from node voltages rather than solved for.
//
// This predicate is THE authority on which elements register a branch, and so
// on which names resolve_branch can later answer for. I() and .save() gate on
// it (directives.go), and the elaborator mirrors it in branchBearing
// (elaborator/elements.go) to validate CCCS/CCVS sense references. If those
// copies ever drift apart, generated code calls resolve_branch on a branch
// nobody registered and stamps into row -1.
func branchBearingType(primType string) bool {
	switch strings.ToLower(primType) {
	case "l", "inductor", "v", "voltage_source", "e", "vcvs", "h", "ccvs":
		return true
	}
	return false
}

// emitStampCall emits the pass-2 stamping call for one physical primitive,
// dispatching on its Type to the matching stamp_* runtime function (see
// mna/matrices.hpp). node(i) and param(i) are small local closures that
// resolve a primitive's i-th terminal/parameter into the C++ expression to
// pass at the call site.
func (g *generator) emitStampCall(phys elaborator.PhysicalObject) {
	node := func(i int) string {
		if i >= len(phys.Nodes) || phys.Nodes[i] == "gnd" || phys.Nodes[i] == "0" {
			return "-1"
		}
		return fmt.Sprintf(`sim.resolve_node(%s)`, cStr(phys.Nodes[i]))
	}

	param := func(i int) string {
		key := fmt.Sprintf("param%d", i)
		if val, ok := phys.Parameters[key]; ok {
			return fmt.Sprintf("%.17g", val)
		}
		return "0.0"
	}

	name := cStr(phys.Name)

	switch strings.ToLower(phys.Type) {
	case "r", "resistor":
		g.line("sim.stamp_resistor(%s, %s, %s);",
			node(0), node(1), param(0))

	case "c", "capacitor":
		g.line("sim.stamp_capacitor(%s, %s, %s);",
			node(0), node(1), param(0))

	case "l", "inductor":
		g.line("{ int br = sim.resolve_branch(%s); sim.stamp_inductor(%s, %s, %s, br); }",
			name, node(0), node(1), param(0))

	case "voltage_source", "v":
		g.line("{ int br = sim.resolve_branch(%s); sim.stamp_voltage_source(%s, %s, %s, br); }",
			name, node(0), node(1), param(0))
		if ac, ph, ok := acExcitation(phys); ok {
			g.line("sim.stamp_ac_voltage(sim.resolve_branch(%s), %.17g, %.17g);", name, ac, ph)
		}

	case "current_source", "i":
		g.line("sim.stamp_current_source(%s, %s, %s, %s);",
			node(0), node(1), param(0), name)
		if ac, ph, ok := acExcitation(phys); ok {
			g.line("sim.stamp_ac_current(%s, %s, %.17g, %.17g);", node(0), node(1), ac, ph)
		}

	case "vccs", "g":
		g.line("sim.stamp_vccs(%s, %s, %s, %s, %s);",
			node(0), node(1), node(2), node(3), param(0))

	case "vcvs", "e":
		g.line("{ int br = sim.resolve_branch(%s); sim.stamp_vcvs(%s, %s, %s, %s, %s, br); }",
			name, node(0), node(1), node(2), node(3), param(0))

	// SenseElement carries the name of the branch whose current is sensed —
	// the last [] entry, resolved by the elaborator against the flattened
	// design (elaborator/elements.go: resolveSenseElements). Reaching here
	// with it empty means elaboration let an unresolved reference through,
	// which is a compiler bug rather than a user error; the emitted #error
	// makes that loud instead of silently producing a circuit missing one
	// element.
	case "cccs", "f":
		if phys.SenseElement == "" {
			g.line("#error unresolved sense element for CCCS %s", phys.Name)
			break
		}
		g.line("{ int sens = sim.resolve_branch(%s); sim.stamp_cccs(%s, %s, sens, %s); }",
			cStr(phys.SenseElement), node(0), node(1), param(0))

	case "ccvs", "h":
		if phys.SenseElement == "" {
			g.line("#error unresolved sense element for CCVS %s", phys.Name)
			break
		}
		g.line("{ int sens = sim.resolve_branch(%s); int br = sim.resolve_branch(%s); sim.stamp_ccvs(%s, %s, sens, %s, br); }",
			cStr(phys.SenseElement), name, node(0), node(1), param(0))
	}
}

// acExcitation returns a source's ac= magnitude and ac_phase= (degrees), and
// whether there is any excitation to stamp. Both are named <...> parameters
// folded to numbers by the elaborator (bindPrimitiveArgs); absent means 0.
func acExcitation(phys elaborator.PhysicalObject) (mag, phaseDeg float64, ok bool) {
	mag = phys.Parameters["ac"]
	phaseDeg = phys.Parameters["ac_phase"]
	return mag, phaseDeg, mag != 0
}
