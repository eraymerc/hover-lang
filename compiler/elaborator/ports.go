package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"path/filepath"
	"sort"
)

// ─────────────────────────────────────────────────────────────────────────────
// MODULE PORT NAMESPACE
//
// A module header declares three kinds of name — static parameters <>, logic
// ports (), and physical ports [] — and all three land in ONE namespace, both
// in semantic analysis (a single scope per module) and in the elaborator (a
// single childPorts map per instance). Nothing enforced that they were
// distinct, and the collision was silent in the worst possible way: the maps
// are filled parameters → logic → physical, so the last writer won and a
// physical port quietly took over a logic port's name.
//
// The failure that motivated this:
//
//     module PWM_PIN<>(input double sig)[wire sig, wire common] {
//         voltage_source SIG_PIN<>(sig)[sig, common];
//     }
//
// The control `(sig)` was meant to be the logic input, but resolved to the
// WIRE, so the source's CtrlSignal became the net's flattened name. Codegen's
// phase_b mangled that into an identifier and emitted
// api_set_voltage_source(..., v4main4ctrl) — an undeclared identifier, in
// generated C++, naming a net the user never typed, three layers away from the
// header line that actually caused it.
//
// There is no correct reading of a duplicated port name, so this is an error
// rather than a resolution rule.
// ─────────────────────────────────────────────────────────────────────────────

// checkModulePortNames rejects any module whose header declares one name twice.
//
// Runs over allModules() — every declaration in the whole loaded set, not just
// the reachable ones and not just the entry file's. Semantic analysis walks the
// entry file alone, so a library module with a colliding header would otherwise
// reach codegen unchecked; and checking declarations rather than instantiations
// means an unused module still gets diagnosed, which is what a user editing
// that library wants.
func (e *Elaborator) checkModulePortNames() {
	mods := e.allModules()
	// allModules() iterates a map, so sort for a stable error order.
	sort.Slice(mods, func(i, j int) bool {
		if e.declFile[mods[i]] != e.declFile[mods[j]] {
			return e.declFile[mods[i]] < e.declFile[mods[j]]
		}
		return mods[i].Line() < mods[j].Line()
	})

	for _, mod := range mods {
		// seen maps a declared name to the kind of port that claimed it, so
		// the message can say WHICH two declarations conflict — "'sig' is
		// declared as both a logic port and a physical port" is actionable in
		// a way that "duplicate name" is not.
		seen := make(map[string]string)
		check := func(name, kind string) {
			if prev, dup := seen[name]; dup {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: module '%s'%s declares '%s' as both %s and %s — "+
						"a module's <> parameters, () logic ports and [] physical ports share one "+
						"namespace, and a duplicate silently resolved to the physical port. Rename one of them.",
					mod.Line(), mod.Name, e.describeDeclFile(mod), name, prev, kind))
				return
			}
			seen[name] = kind
		}

		for _, p := range mod.StaticArgs {
			check(p.Name, "a <> parameter")
		}
		for _, a := range mod.LogicArgs {
			check(a.Name, "a () logic port")
		}
		for _, p := range mod.PhysPorts {
			check(p, "a [] physical port")
		}
	}
}

// checkSourceControls verifies that every driven source's controlling
// expression names a logic signal.
//
// This is the backstop for the same class of bug checkModulePortNames fixes at
// its source, and it catches the direct spelling too — `voltage_source<>(vout)`
// where vout is a wire, which is an easy thing to write and reads perfectly
// sensibly. Without it the mistake is invisible until the C++ compiler reports
// an undeclared `v4main4vout`, because codegen's phase_b mangles CtrlSignal into
// an identifier without ever checking that anything declares it.
//
// Post-flatten by necessity: Symbols is only complete once the whole hierarchy
// has been walked, and a control may name a signal declared in a parent
// instance further up.
func (e *Elaborator) checkSourceControls() {
	for _, p := range e.output.Physicals {
		if p.CtrlSignal == "" {
			continue
		}

		typ, declared := e.output.Symbols[p.CtrlSignal]
		switch {
		case !declared:
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: %s '%s' is controlled by '%s', which is not a declared logic signal.\n"+
					"  A source's () control must be a state/logic variable, or a logic port carrying one.\n"+
					"  For a constant, use the <> parameter form instead: %s<5>()[...]",
				p.Line, p.Type, p.Name, p.CtrlSignal, p.Type))

		case typ.IsWire():
			// The most common way to reach here is a header whose logic port
			// and physical port share a name (checkModulePortNames reports
			// that first and more precisely); the rest is writing the net
			// where the signal was meant.
			e.errors = append(e.errors, fmt.Sprintf(
				"Line %d: %s '%s' is controlled by '%s', which is a wire, not a logic signal.\n"+
					"  A net has no value a source can be driven by — read it into a logic variable first:\n"+
					"      double ctrl = V(%s);\n"+
					"      %s<>(ctrl)[...];",
				p.Line, p.Type, p.Name, p.CtrlSignal, p.CtrlSignal, p.Type))
		}
	}
}

// describeDeclFile renders " (in pmsm.hvr)" for a module from a non-entry file,
// and "" otherwise. A header error in an imported library is otherwise reported
// as a bare line number in a file the user never opened.
func (e *Elaborator) describeDeclFile(mod *ast.ModuleDeclStatement) string {
	path, ok := e.declFile[mod]
	if !ok || path == "" || path == e.entryFile {
		return ""
	}
	return " (in " + filepath.Base(path) + ")"
}
