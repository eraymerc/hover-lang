package codegen

import (
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
			cfg.tEnd = fmt.Sprintf("%.17g", elaborator.ParseEngineering(d.Args[1].String()))
			cfg.tStep = fmt.Sprintf("%.17g", elaborator.ParseEngineering(d.Args[2].String()))

			if len(d.Args) >= 4 {
				maxStepVal := elaborator.ParseEngineering(d.Args[3].String())
				if maxStepVal == 0 {
					return cfg, fmt.Errorf(
						".tran(...)'s 4th argument (max_step) is 0 — a zero-size step is not valid. " +
							"Omit the argument entirely if you don't want an explicit max_step ceiling.")
				}
				cfg.maxStep = solverParam{value: fmt.Sprintf("%.17g", maxStepVal), present: true}
			}
		}
		if d.Name == "solver" && len(d.Args) >= 1 {
			cfg.solverType = d.Args[0].String()
			cfg.rtol = parseOptionalSolverArg(d.Args, 1)
			cfg.atol = parseOptionalSolverArg(d.Args, 2)
			cfg.maxIter = parseOptionalSolverArg(d.Args, 3)
		}
		if d.Name == "zcd" {
			cfg.zcdEnabled = true
		}
		if d.Name == "op" {
			cfg.opEnabled = true
		}
	}

	var saveErr error
	cfg.saveMNANodes, cfg.saveVMSignals, saveErr = g.parseSaveDirectives()
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
func parseOptionalSolverArg(args []ast.Expression, idx int) solverParam {
	if idx >= len(args) {
		return solverParam{present: false}
	}
	val := elaborator.ParseEngineering(args[idx].String())
	if val == 0 {
		return solverParam{present: false}
	}
	return solverParam{value: fmt.Sprintf("%.17g", val), present: true}
}

// parseSaveDirectives extracts every .save(...) argument and classifies
// each one as either an MNA node (a wire that's a terminal of some
// physical primitive — read straight from the solution vector) or a VM
// signal (a logic variable — copied into vm->values by phase_log first).
//
// A name that is NEITHER is a hard error, not a silent fallthrough. The
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
func (g *generator) parseSaveDirectives() (mnaNodes []string, vmSignals []string, err error) {
	for _, d := range g.prog.Directives {
		if d.Name != "save" {
			continue
		}
		for _, arg := range d.Args {
			// arg.String() may return "(main . lc_out)" with spaces and parens
			// from the AST BinaryExpression printer — clean it to "main.lc_out"
			name := cleanString(arg.String())

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

			symType, declared := g.prog.Symbols[name]
			if declared && symType.IsWire() {
				return nil, nil, fmt.Errorf(
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

			return nil, nil, fmt.Errorf(
				".save(%s): no MNA node or logic signal with this name exists — "+
					"did you mean one of these?\n  MNA nodes:    %s\n  Logic signals: %s",
				name,
				joinSortedOrNone(saveableNodeNames(g.prog.Physicals)),
				joinSortedOrNone(saveableSignalNames(g.prog.Symbols)))
		}
	}
	return mnaNodes, vmSignals, nil
}

// physNodeSet builds the set of every (cleaned) node name that appears as
// a terminal of some physical primitive — the names logger_log_step can
// actually resolve to a solution-vector index.
func physNodeSet(physicals []elaborator.PhysicalObject) map[string]bool {
	set := make(map[string]bool)
	for _, phys := range physicals {
		for _, n := range phys.Nodes {
			set[cleanString(n)] = true
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
