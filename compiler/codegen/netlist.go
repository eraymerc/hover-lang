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
	g.raw(`static void register_netlist(System *sys) {`)
	g.push()

	for _, phys := range g.prog.Physicals {
		g.emitRegisterCall(phys)
	}

	g.pop()
	g.raw("}")
	g.raw("")

	// ── Pass 2: stamp values into the now-correctly-sized matrices ────────────────
	g.raw(`// ── NETLIST PASS 2: stamp values ─────────────────────────────────────────────`)
	g.raw(`static void stamp_netlist(System *sys) {`)
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
			g.line("sys->register_node(%s);", cStr(cleanN))
		}
	}
	// Register branch if this primitive needs one
	t := strings.ToLower(phys.Type)
	if t == "l" || t == "inductor" || t == "v" || t == "voltage_source" ||
		t == "e" || t == "vcvs" || t == "h" || t == "ccvs" {
		g.line("sys->register_branch(%s);", cStr(phys.Name))
	}
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
		return fmt.Sprintf(`sys->resolve_node(%s)`, cStr(phys.Nodes[i]))
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
		g.line("stamp_resistor(sys, %s, %s, %s);",
			node(0), node(1), param(0))

	case "c", "capacitor":
		g.line("stamp_capacitor(sys, %s, %s, %s);",
			node(0), node(1), param(0))

	case "l", "inductor":
		g.line("{ int br = sys->resolve_branch(%s); stamp_inductor(sys, %s, %s, %s, br); }",
			name, node(0), node(1), param(0))

	case "voltage_source", "v":
		g.line("{ int br = sys->resolve_branch(%s); stamp_voltage_source(sys, %s, %s, %s, br); }",
			name, node(0), node(1), param(0))

	case "current_source", "i":
		g.line("stamp_current_source(sys, %s, %s, %s, %s);",
			node(0), node(1), param(0), name)

	case "vccs", "g":
		g.line("stamp_vccs(sys, %s, %s, %s, %s, %s);",
			node(0), node(1), node(2), node(3), param(0))

	case "vcvs", "e":
		g.line("{ int br = sys->resolve_branch(%s); stamp_vcvs(sys, %s, %s, %s, %s, %s, br); }",
			name, node(0), node(1), node(2), node(3), param(0))

	case "cccs", "f":
		// SenseElement carries the name of the branch whose current is sensed.
		// Not yet supported by the elaborator — emit a comment as a placeholder.
		if phys.SenseElement != "" {
			g.line("{ int sens = sys->resolve_branch(%s); stamp_cccs(sys, %s, %s, sens, %s); }",
				cStr(phys.SenseElement), node(0), node(1), param(0))
		} else {
			g.line("/* CCCS %s: sense element unknown — not yet supported by elaborator */", phys.Name)
		}

	case "ccvs", "h":
		if phys.SenseElement != "" {
			g.line("{ int sens = sys->resolve_branch(%s); int br = sys->resolve_branch(%s); stamp_ccvs(sys, %s, %s, sens, %s, br); }",
				cStr(phys.SenseElement), name, node(0), node(1), param(0))
		} else {
			g.line("/* CCVS %s: sense element unknown — not yet supported by elaborator */", phys.Name)
		}
	}
}
