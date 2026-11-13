package codegen

import (
	"sort"

	"hover/compiler/token"
)

// emitZcdWatch emits phase_zcd_watch: every scalar that logic (digital or
// structural) writes and analog code reads, published to .zcd through
// sim.zcd_watch_value. .zcd looks for jumps in what the logic drives into the
// circuit, and a value that reaches the circuit only through analog code — an
// analog switch model's `input double g` — is not in the drive vector: the
// analog code that turns it into a current does not run in a .zcd probe. With
// it on this list the edge is found anyway (runtime/vm/zcd.cpp, probe), and a
// model no longer needs to copy its command into a dummy "witness" source.
// Sets g.zcdWatch to the number of values, 0 when there are none.
func (g *generator) emitZcdWatch() {
	analogReads := map[string]bool{}
	logicWrites := map[string]bool{}
	for _, l := range g.prog.Logic {
		switch l.Domain {
		case token.ANALOG:
			for _, r := range l.Reads {
				analogReads[r] = true
			}
		case token.DIGITAL, token.MODULE:
			for _, w := range l.Writes {
				logicWrites[w] = true
			}
		}
	}
	var names []string
	for w := range logicWrites {
		if !analogReads[w] {
			continue
		}
		ht := g.typeOf(mangle(w))
		if ht.isArray() || ht.isPointer() || ht.elem == CStruct {
			continue
		}
		names = append(names, w)
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)

	g.raw(`// ── ZCD WATCH — logic values that reach the circuit through analog code ──────`)
	g.raw(`static void phase_zcd_watch(hvr::Sim &sim) {`)
	g.push()
	for i, w := range names {
		m := mangle(w)
		g.line("sim.zcd_watch_value(%d, %s);", i, emitCast(m, g.typeOf(m).elem, CDouble))
	}
	g.pop()
	g.raw(`}`)
	g.raw(``)
	g.zcdWatch = len(names)
}
