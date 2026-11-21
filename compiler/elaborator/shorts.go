package elaborator

import (
	"fmt"
	"hover/compiler/token"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// ZERO-OHM RESISTORS: IDEAL SHORTS, MERGED AWAY
//
// R<0> means an ideal short. Its value is a compile-time constant — <...>
// arguments are folded to numbers during flattening (bindPrimitiveArgs), and
// codegen stamps them as literals, so nothing at run time (HVR_set_param_*
// included) can move a resistor onto or off zero. The short is therefore
// resolved here, once: the two nets of every zero resistor become one net,
// and the resistor stamps nothing (codegen skips it, netlist.go). The matrix
// gets one row smaller per short, and the short is exact — no 1/R = inf, and
// no large conductance standing in for it.
//
// Rejected alternative: lowering R<0> to a 0 V voltage source. Measured on
// the same decks it gave the same Newton iterations and results to the CSV's
// seven digits, but every short then ADDS a branch row instead of removing a
// node row: 10-30 % more CPU on the BJT examples (each card's Re is 0).
//
// What survives the merge:
//   - Names. Every merged net is recorded in NetAlias (name -> the net it was
//     merged into, "gnd" if any member was ground). Codegen maps V() reads
//     through it and emits sim.alias_node() so .save, .ac and hovercraft
//     outputs keep both names, each column under its own name.
//   - The element. A merged R stays in Physicals with both terminals on the
//     surviving net, so I()/.save diagnostics still know it is a resistor
//     (and still say an R has no branch current — true for R<0> as well).
//   - Other elements across the short. An R, C, L, current source or VCCS
//     whose terminals collapse onto one net is shorted out, as it would be by
//     a wire. An inductor shorted this way keeps the current it had (zero
//     from an uncharged start): zero volts across it, d(i)/dt = 0.
//
// What is an error: a voltage-defined element — a constant or logic-driven
// voltage_source, a VCVS, a CCVS — whose output terminals the shorts join,
// directly or around a loop of other such elements. Its branch equation then
// fixes a voltage across zero ohms and has no solution (the runtime's 1 uOhm
// branch regularisation would turn it into a megaampere answer rather than a
// failure). A voltage source driven by ANALOG code is left alone: the stdlib
// builds impedances that way (v = R * I(br) + ddt(L * I(br)) in <passives>),
// and shorting one of those is a well-posed circuit. The rule mirrors the
// islands check's: report only what the compiler can prove.
//
// Runs after every check that reads the unmerged netlist (sense resolution,
// name collisions, floating islands — where R<0> is an ordinary DC path).
// ─────────────────────────────────────────────────────────────────────────────

// isZeroResistor reports whether p is an R whose value folded to exactly 0.
// Negative values are left to the resistor stamp, as before.
func isZeroResistor(p PhysicalObject) bool {
	t := strings.ToLower(p.Type)
	return (t == "r" || t == "resistor") && len(p.Nodes) == 2 && p.Parameters["param0"] == 0
}

// IsZeroResistor is isZeroResistor for codegen: such an element was merged
// away and must not be registered or stamped.
func IsZeroResistor(p PhysicalObject) bool { return isZeroResistor(p) }

// rigidVoltageElement reports whether p fixes the voltage across its first
// two terminals by an equation that cannot also hold with them shorted.
func rigidVoltageElement(p PhysicalObject, analogDriven map[string]bool) bool {
	if len(p.Nodes) < 2 {
		return false
	}
	switch strings.ToLower(p.Type) {
	case "v", "voltage_source":
		return p.CtrlSignal == "" || !analogDriven[p.CtrlSignal]
	case "e", "vcvs", "h", "ccvs":
		return true
	}
	return false
}

func (e *Elaborator) mergeZeroResistors() {
	physs := e.output.Physicals

	// Union-find over nets, ground spelled one way. The surviving name of a
	// merged group is ground if ground is in it, otherwise the group's net
	// that appears first in the netlist.
	firstSeen := map[string]int{}
	for i, p := range physs {
		for _, n := range p.Nodes {
			if _, ok := firstSeen[n]; !ok && !isGroundNet(n) {
				firstSeen[n] = i
			}
		}
	}
	newUF := func() (map[string]string, func(string) string) {
		parent := map[string]string{}
		var find func(string) string
		find = func(n string) string {
			if isGroundNet(n) {
				return "gnd"
			}
			p, ok := parent[n]
			if !ok || p == n {
				return n
			}
			r := find(p)
			parent[n] = r
			return r
		}
		return parent, find
	}
	union := func(parent map[string]string, find func(string) string, a, b string) bool {
		ra, rb := find(a), find(b)
		if ra == rb {
			return false
		}
		switch {
		case ra == "gnd":
			parent[rb] = ra
		case rb == "gnd":
			parent[ra] = rb
		case firstSeen[rb] < firstSeen[ra]:
			parent[ra] = rb
		default:
			parent[rb] = ra
		}
		return true
	}

	parent, find := newUF()
	var shorts []int
	for i, p := range physs {
		if isZeroResistor(p) {
			shorts = append(shorts, i)
			union(parent, find, p.Nodes[0], p.Nodes[1])
		}
	}
	if len(shorts) == 0 {
		return
	}

	// A voltage-defined element whose terminals the shorts join: walk the
	// rigid elements twice, over the merged nets and over the original ones.
	// An element that closes a loop only in the merged walk closes it through
	// at least one short.
	analogDriven := map[string]bool{}
	for _, l := range e.output.Logic {
		if l.Domain == token.ANALOG {
			for _, w := range l.Writes {
				analogDriven[w] = true
			}
		}
	}
	mParent, mFind := newUF()
	oParent, oFind := newUF()
	for _, p := range physs {
		if !rigidVoltageElement(p, analogDriven) {
			continue
		}
		a, b := find(p.Nodes[0]), find(p.Nodes[1])
		closesMerged := !union(mParent, mFind, a, b)
		closesOrig := !union(oParent, oFind, p.Nodes[0], p.Nodes[1])
		if !closesMerged || closesOrig {
			continue
		}
		how := "directly"
		if a != b {
			how = "around a loop through other voltage sources"
		}
		// The shorts on the nets involved, for the message.
		var culprits []string
		for _, i := range shorts {
			s := physs[i]
			if mFind(find(s.Nodes[0])) == mFind(a) {
				culprits = append(culprits, fmt.Sprintf("%s (line %d)", s.Name, s.Line))
			}
		}
		sort.Strings(culprits)
		if len(culprits) > 4 {
			culprits = append(culprits[:4], "...")
		}
		e.errors = append(e.errors, fmt.Sprintf(
			"Line %d: %s '%s' is short-circuited: zero-ohm resistor(s) %s join its terminals %s.\n"+
				"  R<0> is an ideal short, and a voltage source across an ideal short has no solution.\n"+
				"  Give the resistor a non-zero value, or remove it.",
			p.Line, p.Type, p.Name, strings.Join(culprits, ", "), how))
	}

	alias := map[string]string{}
	for n := range parent {
		if r := find(n); r != n {
			alias[n] = r
		}
	}
	for i := range physs {
		for j, n := range physs[i].Nodes {
			if r, ok := alias[n]; ok {
				physs[i].Nodes[j] = r
			}
		}
	}
	e.output.NetAlias = alias
}
