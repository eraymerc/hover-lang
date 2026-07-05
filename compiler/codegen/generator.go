package codegen

import (
	"fmt"
	"hover/compiler/elaborator"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// ENTRY POINT
// ─────────────────────────────────────────────────────────────────────────────

// Generate walks an ElaboratedProgram and returns the full content of
// sim.cpp, or an error if the program specifies something codegen cannot
// honor — currently this is only an unknown .solver(...) name (see
// solverStructName in util.go), but the error return exists so future
// codegen-time validation failures have an obvious place to report
// through rather than silently degrading or panicking.
func Generate(prog *elaborator.ElaboratedProgram) (string, error) {
	out, _, err := GenerateWithDiagnostics(prog)
	return out, err
}

// GenerateWithDiagnostics is Generate plus a list of every function name
// referenced in the program that could not be resolved to a known Hover
// function (built-in or user-defined). Callers that have visibility into
// the full set of loaded files (the loader's transitive file graph — see
// Interpreter/loader) can use this list to print a real "undefined
// function 'x' — found in y.hvr, add: import \"y.hvr\";" error, rather
// than letting an unresolved call surface only as a C++ compiler error
// three layers removed from the actual Hover-level mistake.
func GenerateWithDiagnostics(prog *elaborator.ElaboratedProgram) (string, []string, error) {
	g := &generator{prog: prog}
	if err := g.emit(); err != nil {
		return "", nil, err
	}
	return g.sb.String(), g.unresolvedFunctions, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GENERATOR STATE
// ─────────────────────────────────────────────────────────────────────────────

type generator struct {
	prog   *elaborator.ElaboratedProgram
	sb     strings.Builder
	indent int

	// typeTable caches collectVarTypes()'s result — built lazily on first
	// use via typeOf (see collect.go), since multiple emit* functions need
	// it and it's a non-trivial AST walk we don't want repeated per call.
	// was: typeTable map[string]CType
	typeTable map[string]hoverType

	// unresolvedFunctions collects every function name emitUserFunctionCall
	// couldn't resolve, in the order first encountered (deduplicated). The
	// caller (which has the loader's full transitively-loaded file set —
	// codegen itself never sees files beyond what the entry elaboration
	// merged into scope) uses this after Generate() returns to print a
	// proper "undefined function, found in <file>, add this import" error
	// instead of letting the problem surface three layers removed as a
	// C++ compiler error on a /* UNRESOLVED_FUNCTION_x */ marker.
	unresolvedFunctions []string

	// usingPrevAPI is set true for the duration of emitting nr_prev(...)'s
	// argument expression, and checked only at V()/I() call emission
	// (expressions.go). This routes every V/I read nested inside an
	// nr_prev(...) call to api_V_prev/api_I_prev instead of api_V/api_I,
	// without needing to thread a new parameter through every recursive
	// emitExpr call site (binary, unary, etc.) — nr_prev is the only
	// construct that changes this, and it always restores the previous
	// value before returning, including across nested nr_prev() calls
	// (which would be unusual but should still nest correctly rather than
	// silently doing the wrong thing).
	usingPrevAPI bool
}

// emit drives the full sim.cpp generation pipeline, section by section.
// Order matters: emitStateVars/emitFunctions must come before the phase
// emitters since phase bodies reference globals and functions declared
// there; emitMain must come last since it wires together everything the
// earlier sections defined.
func (g *generator) emit() error {
	g.emitHeader()
	g.emitStateVars()
	g.emitFunctions()
	g.emitPhaseStructural()
	g.emitPhaseDigital()
	g.emitPhaseAnalog()
	g.emitPhaseB()
	g.emitStateVarSnapshot()
	g.emitBuildNetlist()
	return g.emitMain()
}

// ─────────────────────────────────────────────────────────────────────────────
// LINE-BUFFER MECHANICS
// Shared by every emit* function across all files in this package.
// ─────────────────────────────────────────────────────────────────────────────

// line writes an indented, formatted line followed by a newline.
func (g *generator) line(format string, args ...interface{}) {
	g.sb.WriteString(strings.Repeat("    ", g.indent))
	g.sb.WriteString(fmt.Sprintf(format, args...))
	g.sb.WriteByte('\n')
}

// raw writes a line with no indentation applied — used for section banners
// and top-level constructs that should always start at column 0.
func (g *generator) raw(s string) {
	g.sb.WriteString(s)
	g.sb.WriteByte('\n')
}

func (g *generator) push() { g.indent++ }
func (g *generator) pop()  { g.indent-- }
