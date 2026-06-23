package codegen

import (
	"fmt"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// SMALL UTILITIES
// ─────────────────────────────────────────────────────────────────────────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// quotedList joins names into a comma-separated list of C string literals,
// suitable for a std::vector<std::string> initializer list.
func quotedList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return strings.Join(quoted, ", ")
}

// knownSolvers lists every Hover .solver(...) name this compiler actually
// supports, mapped to its C++ SolverStrategy struct name. This is the only
// place that mapping is allowed to live — solverStructName below is a thin
// wrapper that also enforces "unknown name is a hard error", not a
// silent fallback.
var knownSolvers = map[string]string{
	"euler_fixed":       "EulerFixed",
	"euler_adaptive":    "EulerAdaptive",
	"gauss_siedel":      "GaussSiedel",
	"trapezoidal":       "Trapezoidal",
	"trapezoidal_fixed": "TrapezoidalFixed",
	"bdf2":              "BDF2",
	"ndf2":              "NDF2",
}

// solverFields lists which optional tuning fields (rtol, atol, max_iter,
// max_dt) each C++ solver struct actually declares. Used by
// emitSolverTuningOverrides to silently ignore arguments that don't apply
// to the active solver — e.g. EulerFixed has none of these fields, so
// .solver(euler_fixed, 1e-4, 1e-7, 50) must not emit "strategy.rtol = ...;"
// at all, since that would be a real C++ compile error ("no member named
// 'rtol'"), not a harmless no-op. Matching SPICE's .OPTIONS behavior
// (unused parameters for the active method are silently ignored) requires
// codegen to know, struct by struct, which fields actually exist — there
// is no way to express "ignore if absent" in the generated C++ itself.
var solverFields = map[string]struct {
	rtol, atol, maxIter, maxDt bool
}{
	"EulerFixed":       {false, false, false, false},
	"EulerAdaptive":    {true, true, true, true},
	"GaussSiedel":      {true, true, true, false},
	"Trapezoidal":      {true, true, true, true},
	"TrapezoidalFixed": {true, true, true, false},
	"BDF2":             {true, true, true, true},
	"NDF2":             {true, true, true, true},
}

// emitSolverTuningOverrides emits "strategy.field = value;" for each of
// .tran's max_step and .solver's rtol/atol/max_iter, but only when BOTH:
//   - the argument was actually given (present, per the zero-means-default
//     convention — see solverParam in directives.go), AND
//   - the active solver's C++ struct actually declares that field (per
//     solverFields above).
//
// Either condition failing means no line is emitted for that field at
// all — there is no "emit but make it a no-op" option in C++ for a
// nonexistent struct member, so the only correct behavior is to never
// reference it in generated code.
func (g *generator) emitSolverTuningOverrides(strategyStruct string, cfg simConfig) {
	fields, known := solverFields[strategyStruct]
	if !known {
		// Should not happen — solverStructName only ever returns names
		// present in knownSolvers, which should always have a matching
		// entry here too. If this fires, it's a maintenance gap (a new
		// solver was added to knownSolvers without a matching solverFields
		// entry) rather than anything the user did wrong, so silently
		// emitting nothing is the safest fallback rather than guessing.
		return
	}

	if cfg.rtol.present && fields.rtol {
		g.line("strategy.rtol = %s;", cfg.rtol.value)
	}
	if cfg.atol.present && fields.atol {
		g.line("strategy.atol = %s;", cfg.atol.value)
	}
	if cfg.maxIter.present && fields.maxIter {
		g.line("strategy.max_iter = (int)(%s);", cfg.maxIter.value)
	}
	if cfg.maxStep.present && fields.maxDt {
		g.line("strategy.max_dt = %s;", cfg.maxStep.value)
	}
}

// solverStructName maps a Hover .solver(...) name to the matching C++
// SolverStrategy struct name (see solvers/*.hpp in the runtime).
//
// Unknown solver names are a HARD ERROR, not a silent fallback to
// EulerFixed. A misspelled or unsupported solver name (e.g. "ndf"
// instead of "ndf2") previously fell through to EulerFixed with zero
// warning, silently running the wrong numerical method for an entire
// simulation. That failure mode is far worse than refusing to compile —
// see Generate's error return.
func solverStructName(solverType string) (string, error) {
	if structName, ok := knownSolvers[solverType]; ok {
		return structName, nil
	}
	return "", fmt.Errorf(
		"unknown solver %q in .solver(...) directive — no such solver exists. Available solvers: %s",
		solverType, knownSolverNamesSorted())
}

// knownSolverNamesSorted returns the known solver names joined for an
// error message, in a stable order so the message is deterministic
// across runs (Go map iteration order is randomized).
func knownSolverNamesSorted() string {
	names := make([]string, 0, len(knownSolvers))
	for name := range knownSolvers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// helper:
func formatDoubleLiteral(val float64) string {
	s := fmt.Sprintf("%.17g", val)
	// ensure it reads as a double literal in C++, not an int
	if !strings.ContainsAny(s, ".eEnN") { // no decimal, no exponent, not inf/nan
		s += ".0"
	}
	return s
}
