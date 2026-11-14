package elaborator

import (
	"fmt"
	"hover/compiler/token"
	"path/filepath"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NODES WITH NO DC PATH TO GROUND
//
// A set of nets joined to each other but not, through anything conductive, to
// gnd has no defined common-mode voltage: the MNA matrix is singular at DC.
// SPICE stops on it ("no DC path to ground"); Hover's dense LU used to return
// some answer with zero common mode by luck of pivoting and said nothing.
// checkFloatingIslands WARNS — it never adds an element, never fails the
// build, and does not touch anything codegen reads.
//
// The rule is deliberately one-sided: a warning must be right when it fires,
// so anything the compiler cannot prove is NOT a conductor is counted as one.
// What joins two nets (dcPathPairs):
//
//   R, L                 yes. L is a short at DC; in a transient its branch
//                        row V = L di/dt ties the two nodes' difference.
//   voltage_source       yes, constant or driven, digital or analog: its branch
//                        row fixes V(n+) - V(n-), which determines one from the
//                        other. (Stdlib transformer windings are these.)
//   VCVS, CCVS           yes, between the OUTPUT terminals only (same reason).
//                        A VCVS's control terminals are only read, never tied.
//   C                    no. Open at DC. A node reached only through capacitors
//                        (the middle of two series capacitors) is SPICE's
//                        classic floating node: with .op the DC solve has no
//                        equation for it, and without .op its level is set only
//                        by the zero initial charge.
//   current_source       constant (<value>() or a literal in ()): no — it fixes
//                        a current, not a voltage. Driven: yes when any ANALOG
//                        statement writes its control signal, no otherwise.
//                        The stdlib builds every junction and switch as an
//                        analog current source whose conductance (GMIN
//                        included) exists only in the Newton Jacobian, and the
//                        compiler cannot tell such a source from one whose
//                        expression ignores its own terminals — so all of them
//                        count. Digital and structural logic runs once per
//                        step, outside Newton: the Jacobian (autodiff.go, or
//                        the finite-difference one, which re-runs only the
//                        analog phase) sees a source driven by it as a
//                        constant, exactly like a fixed one. Same for a
//                        control nothing in the design writes (a main ()
//                        input set by a hovercraft host).
//   VCCS                 yes only when its control pair IS its output pair and
//                        gm != 0 — then it is a conductance gm. Otherwise no.
//   CCCS                 no: it injects a current set by another branch.
//
// Knowingly NOT caught (counted as conductors although they may not be):
//   - an analog-driven current source whose expression does not depend on its
//     own terminal voltages (a photocurrent, a torque into a shaft node);
//   - an R of zero or negative value, an L of zero — still edges here;
//   - a VCCS whose control pair is its output pair but whose gm makes the
//     node's total conductance cancel.
// ─────────────────────────────────────────────────────────────────────────────

// isGroundNet mirrors codegen's emitStampCall, which stamps these as -1.
func isGroundNet(n string) bool { return n == "gnd" || n == "0" || n == "" }

// dcPathPairs returns the terminal pairs of one primitive that conduct at DC,
// by the rule above. analogDriven reports whether an analog statement writes
// a control signal.
func dcPathPairs(p PhysicalObject, analogDriven map[string]bool) [][2]string {
	if len(p.Nodes) < 2 {
		return nil
	}
	out := [][2]string{{p.Nodes[0], p.Nodes[1]}}
	switch strings.ToLower(p.Type) {
	case "r", "resistor", "l", "inductor", "v", "voltage_source", "e", "vcvs", "h", "ccvs":
		return out
	case "current_source", "i":
		if analogDriven[p.CtrlSignal] {
			return out
		}
	case "vccs", "g":
		if len(p.Nodes) >= 4 && p.Parameters["param0"] != 0 &&
			((p.Nodes[2] == p.Nodes[0] && p.Nodes[3] == p.Nodes[1]) ||
				(p.Nodes[2] == p.Nodes[1] && p.Nodes[3] == p.Nodes[0])) {
			return out
		}
	}
	return nil
}

// checkFloatingIslands appends one warning per group of nets with no DC path
// to gnd. Post-flatten: it needs every element of the design.
func (e *Elaborator) checkFloatingIslands() {
	physs := e.output.Physicals

	// Signals written by analog code, for the driven-current-source rule.
	analogDriven := map[string]bool{}
	for _, l := range e.output.Logic {
		if l.Domain == token.ANALOG {
			for _, w := range l.Writes {
				analogDriven[w] = true
			}
		}
	}

	// Union-find over nets; every ground spelling is one net.
	parent := map[string]string{}
	var find func(string) string
	find = func(n string) string {
		if isGroundNet(n) {
			n = "gnd"
		}
		p, ok := parent[n]
		if !ok {
			parent[n] = n
			return n
		}
		if p == n {
			return n
		}
		r := find(p)
		parent[n] = r
		return r
	}
	var order []string // nets in first-appearance order, for stable output
	for _, p := range physs {
		for _, n := range p.Nodes {
			if _, seen := parent[n]; !seen && !isGroundNet(n) {
				order = append(order, n)
			}
			find(n)
		}
	}
	for _, p := range physs {
		for _, pr := range dcPathPairs(p, analogDriven) {
			a, b := find(pr[0]), find(pr[1])
			if a != b {
				parent[a] = b
			}
		}
	}

	ground := find("gnd")
	islands := map[string][]string{}
	var roots []string
	for _, n := range order {
		r := find(n)
		if r == ground {
			continue
		}
		if _, ok := islands[r]; !ok {
			roots = append(roots, r)
		}
		islands[r] = append(islands[r], n)
	}

	for _, r := range roots {
		e.warnings = append(e.warnings, e.floatingIslandMsg(islands[r], func(n string) bool { return find(n) == r }))
	}
}

// floatingIslandMsg renders the warning for one island.
func (e *Elaborator) floatingIslandMsg(nets []string, onIsland func(string) bool) string {
	sorted := append([]string(nil), nets...)
	sort.Strings(sorted)

	const maxListed = 8
	list := strings.Join(sorted, ", ")
	if len(sorted) > maxListed {
		list = fmt.Sprintf("%s and %d more", strings.Join(sorted[:maxListed], ", "), len(sorted)-maxListed)
	}
	noun := "nodes"
	if len(sorted) == 1 {
		noun = "node"
	}

	// Which instances have an element on the island, and which have one
	// touching a node off it (gnd included) — the second set is what makes a
	// magnetics instance "one side isolated" rather than "wholly floating".
	instOf := func(p PhysicalObject) string { return p.Name[:strings.LastIndex(p.Name, ".")] }
	offIsland := map[string]bool{}
	for _, p := range e.output.Physicals {
		for _, n := range p.Nodes {
			if isGroundNet(n) || !onIsland(n) {
				offIsland[instOf(p)] = true
			}
		}
	}

	// One element on the island to point at: preferably one in the entry file,
	// which is where the user can act on it.
	var where, xfmr string
	whereInEntry := false
	for _, p := range e.output.Physicals {
		touches := false
		for _, n := range p.Nodes {
			if !isGroundNet(n) && onIsland(n) {
				touches = true
				break
			}
		}
		if !touches {
			continue
		}
		inst := instOf(p)
		mod := e.instModule[inst]
		file := e.entryFile
		if mod != nil {
			file = e.declFile[mod]
			if xfmr == "" && offIsland[inst] && isMagneticsFile(file) {
				xfmr = fmt.Sprintf("'%s' (%s)", inst, mod.Name)
			}
		}
		if where == "" || (!whereInEntry && file == e.entryFile) {
			where = fmt.Sprintf("Line %d: %s '%s'", p.Line, p.Type, p.Name)
			whereInEntry = file == e.entryFile
			if !whereInEntry {
				where += " (in " + filepath.Base(file) + ")"
			}
		}
	}

	example := "R<1Meg>() [<node>, gnd];"
	for _, n := range sorted {
		if strings.HasPrefix(n, "main.") && !strings.Contains(n[5:], ".") {
			example = fmt.Sprintf("R<1Meg>() [%s, gnd];", n[5:])
			break
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s with no DC path to gnd: %s\n", noun, list)
	if where != "" {
		fmt.Fprintf(&b, "  %s is on this island.\n", where)
	}
	b.WriteString("  Nothing conductive (R, L, a voltage source, a VCVS/CCVS output) joins this island to gnd,\n" +
		"  so its voltage is undefined and the circuit matrix is singular at DC.\n")
	if xfmr != "" {
		fmt.Fprintf(&b, "  Likely an isolated transformer side: %s couples these nodes to the rest of\n"+
			"  the circuit magnetically, not conductively.\n", xfmr)
	}
	which := "one of these nodes"
	if len(sorted) == 1 {
		which = "it"
	}
	fmt.Fprintf(&b, "  Fix: connect %s to gnd, directly or through a large resistor, e.g. %s", which, example)
	return b.String()
}

// isMagneticsFile: the module was declared in the stdlib's magnetics package
// (Transformer, CoupledInductors, IdealTransformer, ...), whose sides are
// coupled by equations only.
func isMagneticsFile(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == "magnetics" {
			return true
		}
	}
	return false
}
