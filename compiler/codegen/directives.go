package codegen

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
)

// ─────────────────────────────────────────────────────────────────────────────
// SIMULATION DIRECTIVE PARSING
//
// Hover's top-level `.tran(...)`, `.solver(...)`, `.zcd;`, `.op;`, and
// `.save(...)` directives configure the simulation as a whole, independent
// of any module logic. This file extracts that configuration from
// g.prog.Directives into a single struct that emitMain (main_emit.go)
// consumes — keeping "what does the simulation config say" separate from
// "how do I write that config into main()".
// ─────────────────────────────────────────────────────────────────────────────

// solverParam represents one optional .solver(...) tuning argument
// (rtol, atol, or max_iter). present is false when the argument was
// omitted entirely OR explicitly passed as 0 — both mean "use this
// solver's own struct default," and emitMain (main_emit.go) skips
// emitting an override line for any param where present is false.
type solverParam struct {
	value   string // C++ literal text, only meaningful when present
	present bool
}

// simConfig holds everything emitMain needs to know about how the
// simulation should be set up, after parsing all top-level directives.
type simConfig struct {
	tEnd       string      // C++ double literal, e.g. "0.029999999999999999"
	tStep      string      // C++ double literal
	maxStep    solverParam // .tran's optional 4th argument — ignored by fixed-step solvers
	solverType string      // raw Hover solver name, e.g. "ndf2" — resolved to a C++ struct name by solverStructName
	zcdEnabled bool
	opEnabled  bool

	// rtol/atol/maxIter are .solver(...)'s optional 2nd-4th arguments.
	// Unlike .tran's maxStep, 0 here IS a valid sentinel meaning "use the
	// default" (see solverParam) — this is a deliberate asymmetry between
	// the two directives, not an inconsistency: a zero step size is
	// physically meaningless, while a zero tolerance/iteration-count
	// override has no other sensible interpretation than "I didn't mean
	// to override this."
	rtol    solverParam
	atol    solverParam
	maxIter solverParam

	// saveMNANodes are .save() arguments that resolved to an MNA node name
	// (a wire that's also a terminal of some physical primitive) — these
	// are read directly from the solution vector by logger_log_step.
	saveMNANodes []string

	// saveVMSignals are .save() arguments that resolved to a logic
	// variable instead — these are copied into vm->values by phase_log
	// (see emitPhaseLog in phases.go) before logger_log_signals reads them.
	saveVMSignals []string

	// saveBranchCurrents are .save() arguments that resolved to a
	// branch-bearing circuit element — `.save(main.vsense)` or the explicit
	// `.save(I(main.vsense))`. They ride the VM-signal channel rather than
	// needing a logger change of their own: phase_log emits
	// vm->values["I(main.vsense)"] = api_I(...), and the key is appended to
	// the vm_signals list handed to logger_init. That works because every
	// solver calls logger_log_step -> phase_log -> logger_log_signals in that
	// order, after api_update_solution has published the timestep's solution.
	//
	// Held separately from saveVMSignals (rather than pre-mixed) so
	// emitPhaseLog can tell which entries need an api_I call instead of a
	// C-global read.
	saveBranchCurrents []string
}

// loggerSignalList is the vm_signals vector handed to logger_init: the logic
// variables, then the branch-current columns.
//
// Branch columns go LAST so that a design without any of them produces a CSV
// byte-identical to what it produced before this feature existed —
// compare_csv.py compares headers for exact equality, so column order is part
// of the golden-test contract.
func (c simConfig) loggerSignalList() []string {
	sigs := append([]string(nil), c.saveVMSignals...)
	for _, elem := range c.saveBranchCurrents {
		sigs = append(sigs, branchCurrentColumn(elem))
	}
	return sigs
}

// directiveNumber evaluates a directive argument that must be a numeric
// literal (engineering suffixes allowed, unary minus allowed). Anything
// else is an error — the old path stringified the AST and fed it to
// ParseEngineering, whose ParseFloat fallback silently turned a
// non-numeric argument into 0 (the same silent-zero disease evalStatic
// had; see elaborator/evaluate.go).
func directiveNumber(directive string, arg ast.Expression) (float64, error) {
	switch e := arg.(type) {
	case *ast.NumberExpression:
		return elaborator.ParseEngineering(e.Value), nil
	case *ast.UnaryExpression:
		if e.Operator == "-" {
			v, err := directiveNumber(directive, e.Right)
			return -v, err
		}
	}
	return 0, fmt.Errorf(".%s(...): argument '%s' must be a numeric literal", directive, arg.String())
}

// parseSimConfig walks g.prog.Directives once and extracts every directive
// this compiler understands. Directives it doesn't recognize are silently
// ignored — unknown directives are not a compile error today.
//
// Returns an error if .tran's optional 4th argument (max_step) is present
// but evaluates to exactly 0 — a zero-size step is physically meaningless,
// and treating it as a tuning default sentinel here (the way .solver's
// arguments work) would silently produce a simulation that can never
// advance time on adaptive solvers, far more likely to be a typo'd source
// of confusion than the actual asymmetry being a hard error makes it.
func (g *generator) parseSimConfig() (simConfig, error) {
	cfg := simConfig{
		tEnd:       "0.01",
		tStep:      "1e-6",
		solverType: "euler_fixed",
	}

	for _, d := range g.prog.Directives {
		if d.Name == "tran" && len(d.Args) >= 3 {
			tEnd, err := directiveNumber("tran", d.Args[1])
			if err != nil {
				return cfg, err
			}
			tStep, err := directiveNumber("tran", d.Args[2])
			if err != nil {
				return cfg, err
			}
			cfg.tEnd = fmt.Sprintf("%.17g", tEnd)
			cfg.tStep = fmt.Sprintf("%.17g", tStep)

			if len(d.Args) >= 4 {
				maxStepVal, err := directiveNumber("tran", d.Args[3])
				if err != nil {
					return cfg, err
				}
				if maxStepVal == 0 {
					return cfg, fmt.Errorf(
						".tran(...)'s 4th argument (max_step) is 0 — a zero-size step is not valid. " +
							"Omit the argument entirely if you don't want an explicit max_step ceiling.")
				}
				cfg.maxStep = solverParam{value: fmt.Sprintf("%.17g", maxStepVal), present: true}
			}
		}
		if d.Name == "solver" && len(d.Args) >= 1 {
			// The solver name is a bare identifier ("bdf2"); anything else
			// falls through to solverStructName's unknown-solver hard error
			// with the raw source text for the message.
			if id, ok := d.Args[0].(*ast.IdentifierExpression); ok {
				cfg.solverType = id.Value
			} else {
				cfg.solverType = d.Args[0].String()
			}
			var err error
			if cfg.rtol, err = parseOptionalSolverArg(d.Args, 1); err != nil {
				return cfg, err
			}
			if cfg.atol, err = parseOptionalSolverArg(d.Args, 2); err != nil {
				return cfg, err
			}
			if cfg.maxIter, err = parseOptionalSolverArg(d.Args, 3); err != nil {
				return cfg, err
			}
		}
		if d.Name == "zcd" {
			cfg.zcdEnabled = true
		}
		if d.Name == "op" {
			cfg.opEnabled = true
		}
	}

	var saveErr error
	cfg.saveMNANodes, cfg.saveVMSignals, cfg.saveBranchCurrents, saveErr = g.parseSaveDirectives()
	if saveErr != nil {
		return cfg, saveErr
	}
	return cfg, nil
}

// parseOptionalSolverArg reads .solver(...)'s argument at index idx (1 =
// rtol, 2 = atol, 3 = max_iter). Returns present=false if the argument
// wasn't supplied at all, OR if it evaluates to exactly 0 — both cases
// mean "use this solver's own struct default" per the zero-means-default
// convention.
func parseOptionalSolverArg(args []ast.Expression, idx int) (solverParam, error) {
	if idx >= len(args) {
		return solverParam{present: false}, nil
	}
	val, err := directiveNumber("solver", args[idx])
	if err != nil {
		return solverParam{}, err
	}
	if val == 0 {
		return solverParam{present: false}, nil
	}
	return solverParam{value: fmt.Sprintf("%.17g", val), present: true}, nil
}

// parseSaveDirectives extracts every .save(...) argument and classifies each
// one as an MNA node (a wire that's a terminal of some physical primitive —
// read straight from the solution vector), a VM signal (a logic variable —
// copied into vm->values by phase_log first), or a branch current (a named
// branch-bearing element — also routed through vm->values, see
// simConfig.saveBranchCurrents).
//
// A name that is NONE of those is a hard error, not a silent fallthrough. The
// previous behavior classified any unrecognized name as a VM signal; at
// runtime logger_log_signals then found nothing in vm->values and logged
// 0.0 forever — so a typo'd .save argument compiled cleanly and produced
// a plausible-looking all-zeros column in the CSV. Refusing to compile,
// with the full list of what IS saveable, is strictly better. Same
// philosophy as solverStructName's unknown-solver hard error.
//
// A declared-but-unconnected wire is its own distinct error: it exists as
// a symbol, but it has no row in the MNA matrix, so there is no voltage
// to record — logger_log_step would log 0.0 for it just like a typo.
func (g *generator) parseSaveDirectives() (mnaNodes, vmSignals, branchCurrents []string, err error) {
	for _, d := range g.prog.Directives {
		if d.Name != "save" {
			continue
		}
		for _, arg := range d.Args {
			// Explicit current form: .save(I(main.vsense)). Handled before
			// dottedPath, which would reject a call expression outright.
			if call, isCall := arg.(*ast.CallExpression); isCall && callExpressionName(call.Function) == "I" {
				if len(call.Arguments) != 1 {
					return nil, nil, nil, fmt.Errorf(
						".save(%s): I() takes exactly one element name", arg.String())
				}
				elem, ok := dottedPath(call.Arguments[0])
				if !ok {
					return nil, nil, nil, fmt.Errorf(
						".save(%s): I()'s argument must be an element name", arg.String())
				}
				if !g.branchElements()[elem] {
					return nil, nil, nil, errors.New(g.badBranchRefMsg("I", elem, elem))
				}
				branchCurrents = append(branchCurrents, elem)
				continue
			}

			name, ok := dottedPath(arg)
			if !ok {
				return nil, nil, nil, fmt.Errorf(
					".save(%s): argument must be a signal, node, or element name (e.g. main.q1_base)", arg.String())
			}

			// Ground is always saveable (logger special-cases it to 0.0
			// by definition, which for ground is correct, not a fallback).
			if name == "gnd" || name == "0" {
				mnaNodes = append(mnaNodes, name)
				continue
			}

			if physNodeSet(g.prog.Physicals)[name] {
				mnaNodes = append(mnaNodes, name)
				continue
			}

			// Bare element name: .save(main.vsense) on a branch-bearing
			// element logs its current. Element and net names are guaranteed
			// disjoint by the elaborator (checkElementNameCollisions), so this
			// can never shadow the node branch above.
			if g.branchElements()[name] {
				branchCurrents = append(branchCurrents, name)
				continue
			}
			if _, isElement := g.elementByName()[name]; isElement {
				// An element, but one with no branch row — say so and point at
				// the ammeter idiom rather than claiming the name is unknown.
				return nil, nil, nil, errors.New(g.badBranchRefMsg("save", name, name))
			}

			symType, declared := g.prog.Symbols[name]
			if declared && symType.IsWire() {
				return nil, nil, nil, fmt.Errorf(
					".save(%s): '%s' is a declared wire, but it is not connected to any "+
						"physical primitive — it has no MNA matrix row, so there is no "+
						"voltage to record. Connect it to a component or remove it from .save().",
					name, name)
			}
			if declared {
				// VM signals are stored with dots in the values map
				// (e.g. "main.generator.carrierWave")
				vmSignals = append(vmSignals, name)
				continue
			}

			return nil, nil, nil, fmt.Errorf(
				".save(%s): no MNA node, logic signal, or circuit element with this name exists — "+
					"did you mean one of these?\n  MNA nodes:       %s\n  Logic signals:   %s\n  Element currents: %s",
				name,
				joinSortedOrNone(saveableNodeNames(g.prog.Physicals)),
				joinSortedOrNone(saveableSignalNames(g.prog.Symbols)),
				joinSortedOrNone(namedBranchElements(g.prog.Physicals)))
		}
	}
	return mnaNodes, vmSignals, branchCurrents, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ELEMENT LOOKUPS
//
// I() and .save() both need to answer "is this name an element, and does it
// have a branch current?" Both lookups are cached on the generator since
// emitExpr can hit I() many times.
// ─────────────────────────────────────────────────────────────────────────────

// branchElements is the set of element names that own an MNA branch row — the
// only names sys->resolve_branch can answer for, and therefore the only legal
// arguments to I() or to a .save() current column.
func (g *generator) branchElements() map[string]bool {
	if g.branchSet == nil {
		g.branchSet = make(map[string]bool)
		for _, p := range g.prog.Physicals {
			if branchBearingType(p.Type) {
				g.branchSet[p.Name] = true
			}
		}
	}
	return g.branchSet
}

// elementByName maps every element name (branch-bearing or not) to its index
// in prog.Physicals — the codegen-side twin of elaborator.elementIndex. Used
// to tell "that's a resistor, it has no branch" apart from "no such element",
// which are very different things to tell a user.
func (g *generator) elementByName() map[string]int {
	if g.elementIdx == nil {
		g.elementIdx = make(map[string]int, len(g.prog.Physicals))
		for i, p := range g.prog.Physicals {
			g.elementIdx[p.Name] = i
		}
	}
	return g.elementIdx
}

// namedBranchElements lists the elements whose current can be read, for "did
// you mean" suggestions. Restricted to user-named ones: a synthesized
// "main.L_4" is an implementation detail whose number shifts whenever a
// component is added above it, so suggesting it would be bad advice.
func namedBranchElements(physicals []elaborator.PhysicalObject) []string {
	var names []string
	for _, p := range physicals {
		if p.UserNamed && branchBearingType(p.Type) {
			names = append(names, p.Name)
		}
	}
	return names
}

// badBranchRefMsg explains why `name` can't be read as a branch current.
// ctx is the construct that asked: "I" for an I() call in logic, "save" for a
// .save() argument.
//
// The three cases are genuinely different problems with different fixes, and
// collapsing them into one "not found" would send users hunting for a typo
// when what they actually need is an ammeter.
func (g *generator) badBranchRefMsg(ctx, written, resolved string) string {
	// how a reference to `n` is spelled in the construct that asked
	ref := func(n string) string {
		if ctx == "save" {
			return ".save(" + n + ")"
		}
		return "I(" + n + ")"
	}

	if idx, isElement := g.elementByName()[resolved]; isElement {
		// The suggested ammeter is a DECLARATION, so it must be spelled as a
		// bare identifier — a flattened path like "main.rload_amp" is a valid
		// reference but not valid syntax to declare one. The follow-up
		// reference, by contrast, keeps whatever qualification the user's own
		// spelling had: .save() takes full paths, I() takes module-local names.
		bare := written
		if i := strings.LastIndexByte(bare, '.'); i >= 0 {
			bare = bare[i+1:]
		}
		return fmt.Sprintf(
			"%s: '%s' is a %s, which has no branch current in the MNA formulation "+
				"(its current is derived from node voltages, not solved for directly).\n"+
				"  Put a 0 V voltage source in series with it as an ammeter and read that instead:\n"+
				"      voltage_source %s_amp<0>() [<node>, <node>];\n"+
				"  then refer to it as %s",
			ref(written), written, g.prog.Physicals[idx].Type, bare, ref(written+"_amp"))
	}
	if physNodeSet(g.prog.Physicals)[resolved] {
		return fmt.Sprintf(
			"%s: '%s' is a net, not a circuit element — a branch current belongs to an element. "+
				"Did you mean V(%s)?",
			ref(written), written, written)
	}
	return fmt.Sprintf(
		"%s: no circuit element named '%s' exists.\n"+
			"  Name the element you want to read, e.g.  voltage_source vsense<0>() [a, b];\n"+
			"  Elements whose current can be read: %s",
		ref(written), resolved, joinSortedOrNone(namedBranchElements(g.prog.Physicals)))
}

// physNodeSet builds the set of every (cleaned) node name that appears as
// a terminal of some physical primitive — the names logger_log_step can
// actually resolve to a solution-vector index.
func physNodeSet(physicals []elaborator.PhysicalObject) map[string]bool {
	set := make(map[string]bool)
	for _, phys := range physicals {
		for _, n := range phys.Nodes {
			set[n] = true // elaborator-produced node names are already clean dotted paths
		}
	}
	return set
}

// saveableNodeNames returns every MNA node name a .save() could refer to,
// for error-message display. Ground is omitted — it's always saveable but
// listing it as a suggestion for a typo is never helpful.
func saveableNodeNames(physicals []elaborator.PhysicalObject) []string {
	set := physNodeSet(physicals)
	delete(set, "gnd")
	delete(set, "0")
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	return names
}

// saveableSignalNames returns every logic-variable name a .save() could
// refer to — every elaborated symbol that isn't a wire (wires are either
// MNA nodes, listed separately, or unconnected, which is an error).
func saveableSignalNames(symbols map[string]ast.Type) []string {
	names := make([]string, 0, len(symbols))
	for name, symType := range symbols {
		if !symType.IsWire() {
			names = append(names, name)
		}
	}
	return names
}

// joinSortedOrNone renders a name list for an error message in a stable
// order (Go map iteration is randomized), or "(none)" if empty.
func joinSortedOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
