package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"sort"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NAMED CIRCUIT ELEMENTS
//
// Every physical primitive gets a flattened name, which is the string the
// runtime keys its branch_map by. Historically those names were all
// synthesized positionally ("main.R_3") and there was no way to write one in
// source — which is why I() was unusable and why CCCS/CCVS could never say
// whose current they sense, despite the stamps existing all along.
//
// A user name (`R rsense<1m>() [a, b];`) simply substitutes for the
// synthesized segment. Everything downstream — register_branch, resolve_branch,
// api_I, the CCCS/CCVS sense reference — already worked by string, so naming is
// the entire feature.
// ─────────────────────────────────────────────────────────────────────────────

// branchBearing reports whether a primitive type gets its own MNA branch row,
// i.e. whether its current is a solved unknown that CCCS/CCVS can sense and
// I() can read.
//
// MUST stay in lockstep with codegen's branchBearingType, which drives the
// actual register_branch calls (codegen/netlist.go). If the two ever disagree,
// generated code calls resolve_branch on a branch nobody registered and stamps
// into row -1.
//
// R, C, VCCS and current_source are deliberately absent: their current is
// derived from node voltages rather than solved for, so there is no row to
// point at. The supported way to measure those is a 0 V voltage source in
// series as an ammeter — see the error text in resolveSenseElements.
func branchBearing(primType string) bool {
	switch strings.ToLower(primType) {
	case "l", "inductor", "v", "voltage_source", "e", "vcvs", "h", "ccvs":
		return true
	}
	return false
}

// elementPath extracts a dotted name path ("probe.vsense") from a sense
// reference STRUCTURALLY — an IdentifierExpression or a chain of "."
// BinaryExpressions — returning ok=false for anything else.
//
// Emphatically NOT expr.String(): the pretty-printer renders a dotted path as
// "(probe . vsense)", so stringifying and scrubbing the punctuation back out
// would be a shadow parser that silently "succeeds" on expressions that were
// never name paths at all. Mirrors codegen's dottedPath, duplicated because
// the elaborator does not depend on codegen.
func elementPath(expr ast.Expression) (string, bool) {
	switch e := expr.(type) {
	case *ast.IdentifierExpression:
		return e.Value, true
	case *ast.BinaryExpression:
		if e.Operator != "." {
			return "", false
		}
		left, ok := elementPath(e.Left)
		if !ok {
			return "", false
		}
		right, rok := e.Right.(*ast.IdentifierExpression)
		if !rok {
			return "", false
		}
		return left + "." + right.Value, true
	}
	return "", false
}

// elementName produces the flattened name for one physical primitive, and
// reports whether it came from the user.
//
// A user name is taken verbatim under the instance prefix, so a module
// instantiated twice yields main.a.rsense and main.b.rsense — names scope to
// the instance exactly the way wires do.
//
// An unnamed primitive keeps the historical positional scheme, with one
// addition: if that name is already taken — which can only happen if a user
// hand-wrote something like `R R_7<...>` — the index is bumped until it is
// free. Sidestepping the collision beats reporting it, since the user did not
// choose the colliding name and has no way to influence it. The reverse order
// (the user's name written after the synthesized one was handed out) IS
// reported, against the user's line, where they can act on it.
func (e *Elaborator) elementName(s *ast.PhysicalPrimitiveStatement, prefix string, physIdx int) (string, bool) {
	if s.Name != "" {
		name := prefix + "." + s.Name
		if prev, exists := e.elementIndex[name]; exists {
			other := e.output.Physicals[prev]
			if other.UserNamed {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: duplicate element name '%s' — already declared at line %d as a %s",
					s.Line(), s.Name, other.Line, other.Type))
			} else {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: element name '%s' collides with the compiler-generated name for an "+
						"unnamed %s in the same module — pick a name that is not of the form <Type>_<number>",
					s.Line(), s.Name, other.Type))
			}
		}
		return name, true
	}
	for k := physIdx; ; k++ {
		name := prefix + "." + s.PrimType + "_" + strconv.Itoa(k)
		if _, taken := e.elementIndex[name]; !taken {
			return name, false
		}
	}
}

// resolveSenseElements binds each CCCS/CCVS to the branch of its controlling
// element, once the whole design is flattened.
//
// Deferred to a post-pass rather than done inline because a sense reference
// may legitimately point forward — the sensed source is often declared below
// the controlled source in the same module body — and netlist construction
// imposes no ordering (register_netlist runs entirely before stamp_netlist).
func (e *Elaborator) resolveSenseElements() {
	for _, ref := range e.pendingSense {
		ctrl := &e.output.Physicals[ref.physIdx]

		idx, ok := e.elementIndex[ref.target]
		if !ok {
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: %s references an unknown sense element '%s' — no circuit element named '%s' exists.\n"+
					"  Name the element whose current you want to sense, e.g.:\n"+
					"      voltage_source vsense<0>() [a, b];   // 0 V source = ammeter\n"+
					"  Named elements in this design: %s",
				ref.line, ctrl.Type, ref.raw, ref.target, e.namedElementList()))
			continue
		}

		if idx == ref.physIdx {
			// The controlled source's own branch row would reference itself
			// with gain r, which is structurally singular.
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: %s '%s' senses its own branch current",
				ref.line, ctrl.Type, ref.raw))
			continue
		}

		sensed := e.output.Physicals[idx]
		if !branchBearing(sensed.Type) {
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: %s cannot sense the current through '%s' — a %s has no branch current in the "+
					"MNA formulation (its current is derived from node voltages, not solved for directly).\n"+
					"  Put a 0 V voltage source in series with it as an ammeter and sense that instead:\n"+
					"      voltage_source %s_amp<0>() [<node>, <node>];\n"+
					"      %s<...>() [out_p, out_n, %s_amp];",
				ref.line, ctrl.Type, ref.raw, sensed.Type, ref.raw, ctrl.Type, ref.raw))
			continue
		}

		ctrl.SenseElement = sensed.Name
	}
}

// checkElementNameCollisions rejects an element whose flattened name is also a
// net or a logic signal.
//
// The runtime keeps node_map and branch_map separate, so this would not
// corrupt the solve — but .save(x) and V(x)/I(x) could then no longer tell the
// two apart by name, and the CSV would carry two different physical quantities
// under one header. Better to reject at compile time.
//
// Runs post-flatten because a colliding wire may be declared anywhere in the
// hierarchy, including in a file semantic analysis never walks (semantic sees
// only the entry file — see main.go).
func (e *Elaborator) checkElementNameCollisions() {
	nets := make(map[string]bool)
	for _, p := range e.output.Physicals {
		for _, n := range p.Nodes {
			if n != "gnd" && n != "0" {
				nets[n] = true
			}
		}
	}

	for _, p := range e.output.Physicals {
		if !p.UserNamed {
			continue // synthesized names cannot collide; elementName guarantees it
		}
		if nets[p.Name] {
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: element name '%s' is also used as a wire/net in this circuit — rename one of them",
				p.Line, p.Name))
			continue
		}
		if _, isSignal := e.output.Symbols[p.Name]; isSignal {
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: element name '%s' is also used as a logic signal — rename one of them",
				p.Line, p.Name))
		}
	}
}

// namedElementList renders the design's user-named elements for diagnostics.
// Synthesized names are excluded on purpose — suggesting "main.R_3" would be
// telling the user to type an implementation detail that shifts whenever they
// add a component above it.
func (e *Elaborator) namedElementList() string {
	var names []string
	for _, p := range e.output.Physicals {
		if p.UserNamed {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return "(none — no circuit element in this design has a name yet)"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
