package elaborator

import (
	ast "hover/compiler/ast"
	"hover/compiler/token"
)

// ─────────────────────────────────────────────────────────────────────────────
// DATAFLOW ORDER — which module instance runs first within a phase
//
// Flattening appends each instance's statements where the instance is
// written, so a phase used to run its modules in INSTANTIATION order. A module
// that reads a signal another one writes then saw the signal's value from the
// previous step whenever the writer happened to be instantiated after it: a
// PWM dead-time generator instantiated before its modulator delayed every
// gate edge by a step and .zcd could not place it ("failed to converge" at
// the first edge, docs/hoveR_sorunlar.txt item 33), and the stdlib carried a
// "instantiate the modulator first" rule to work around it. Analog modules
// that pass doubles to each other (a reference feeding a bridge) have the
// same problem within the Newton loop.
//
// orderLogicByDataflow reorders BLOCKS — maximal runs of consecutive
// statements from one instance in one phase — so that a block writing a
// signal runs before the blocks of the same phase that read it. A block's own
// statements never move relative to each other, nor do two runs of the same
// instance, nor two writers of one signal. Where the reads form a loop
// (feedback between modules within a step) the loop keeps instantiation order,
// as before. The sort is stable: a deck with no such dependency is unchanged.
// ─────────────────────────────────────────────────────────────────────────────

type logicBlock struct {
	first, last int // range in the original Logic slice, inclusive
	prefix      string
	domain      token.Type
	clock       string
	reads       map[string]bool
	writes      map[string]bool
}

func orderLogicByDataflow(logic []LogicObject) []LogicObject {
	// 1. Blocks.
	var blocks []*logicBlock
	for i, l := range logic {
		if n := len(blocks); n > 0 {
			b := blocks[n-1]
			if b.prefix == l.Prefix && b.domain == l.Domain && b.clock == l.Clock {
				b.last = i
				addLogicRefs(b, l)
				continue
			}
		}
		b := &logicBlock{first: i, last: i, prefix: l.Prefix, domain: l.Domain, clock: l.Clock,
			reads: map[string]bool{}, writes: map[string]bool{}}
		addLogicRefs(b, l)
		blocks = append(blocks, b)
	}
	n := len(blocks)
	if n < 2 {
		return logic
	}

	// 2. Edges, within one phase (domain + clock) only: different phases run
	//    at different points of a step, and their order is the runtime's.
	succ := make([][]int, n)
	indeg := make([]int, n)
	edge := func(a, b int) { succ[a] = append(succ[a], b); indeg[b]++ }
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			if a == b {
				continue
			}
			A, B := blocks[a], blocks[b]
			if A.domain != B.domain || A.clock != B.clock {
				continue
			}
			switch {
			case A.prefix == B.prefix, sharesKey(A.writes, B.writes):
				// Two runs of one instance, or two writers of one signal:
				// they keep their original order.
				if a < b {
					edge(a, b)
				}
			case sharesKey(A.writes, B.reads) && sharesKey(B.writes, A.reads):
				// Each reads what the other writes: a loop within the step.
				// It keeps its original order.
				if a < b {
					edge(a, b)
				}
			case sharesKey(A.writes, B.reads):
				edge(a, b) // A writes what B reads: A first
			}
		}
	}

	// 3. Stable Kahn: of the ready blocks, the earliest original one; on a
	//    cycle (nothing ready), the earliest remaining one.
	done := make([]bool, n)
	order := make([]int, 0, n)
	for len(order) < n {
		pick := -1
		for i := 0; i < n; i++ {
			if !done[i] && indeg[i] == 0 {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i := 0; i < n; i++ {
				if !done[i] {
					pick = i
					break
				}
			}
		}
		done[pick] = true
		order = append(order, pick)
		for _, s := range succ[pick] {
			indeg[s]--
		}
	}

	out := make([]LogicObject, 0, len(logic))
	for _, bi := range order {
		out = append(out, logic[blocks[bi].first:blocks[bi].last+1]...)
	}
	return out
}

// addLogicRefs adds a statement's references to its block. A bare expression
// statement (a function call) lists its arguments as both reads and writes;
// only the reads are used here, so a call that merely passes a signal does not
// count as a second writer of it.
func addLogicRefs(b *logicBlock, l LogicObject) {
	for _, r := range l.Reads {
		b.reads[r] = true
	}
	if _, isCall := l.Source.(*ast.ExpressionStatement); isCall {
		return
	}
	for _, w := range l.Writes {
		b.writes[w] = true
	}
}

func sharesKey(a, b map[string]bool) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
