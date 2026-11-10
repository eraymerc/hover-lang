package codegen

import (
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NAME HANDLES
//
// Every V(), V_prev(), I(), I_prev() read and every driven-source write in the
// phases names a net or element. The by-name runtime calls (sim.V("main.x"))
// build a std::string and hash it on every call, and a numerical Jacobian runs
// the phases n+2 times per formation: on examples/MOSFET/h_bridge_inverter.hvr
// the lookups were 91 % of the run (the MOSFET equations themselves ~1 %).
//
// So each distinct name gets a slot in hvr_h[], filled ONCE by
// resolve_handles() right after the netlist is stamped (current sources are
// registered while stamping), and the phases index it: sim.V(hvr_h[3]). The
// slots are collected while the phases are emitted, so their declaration is
// written at a placeholder after the fact (see finishHandles).
// ─────────────────────────────────────────────────────────────────────────────

const handlesPlaceholder = "/*@@HVR_HANDLES@@*/"

type handleKind int

const (
	handleNode handleKind = iota
	handleBranch
	handleVoltageSource
	handleCurrentSource
)

var handleResolver = map[handleKind]string{
	handleNode:          "node_handle",
	handleBranch:        "branch_handle",
	handleVoltageSource: "voltage_source_handle",
	handleCurrentSource: "current_source_handle",
}

type handleDef struct {
	kind handleKind
	name string
}

// handle returns the C expression for name's slot, allocating it on first use.
func (g *generator) handle(kind handleKind, name string) string {
	key := fmt.Sprintf("%d\x00%s", kind, name)
	if g.handleIdx == nil {
		g.handleIdx = map[string]int{}
	}
	k, ok := g.handleIdx[key]
	if !ok {
		k = len(g.handles)
		g.handleIdx[key] = k
		g.handles = append(g.handles, handleDef{kind, name})
	}
	return fmt.Sprintf("hvr_h[%d]", k)
}

// finishHandles writes the hvr_h[] declaration and resolve_handles() at the
// placeholder, now that every phase has been emitted.
func (g *generator) finishHandles() {
	var b strings.Builder
	b.WriteString("// ── NAME HANDLES — names resolved once, indices used in the phases ──────────\n")
	n := len(g.handles)
	if n == 0 {
		n = 1
	}
	fmt.Fprintf(&b, "static int hvr_h[%d];\n", n)
	b.WriteString("static void resolve_handles(hvr::Sim &sim) {\n")
	b.WriteString("    (void)sim;\n")
	for i, h := range g.handles {
		fmt.Fprintf(&b, "    hvr_h[%d] = sim.%s(%s);\n", i, handleResolver[h.kind], cStr(h.name))
	}
	b.WriteString("}\n")
	out := strings.Replace(g.sb.String(), handlesPlaceholder, b.String(), 1)
	g.sb.Reset()
	g.sb.WriteString(out)
}
