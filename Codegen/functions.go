package codegen

import (
	"fmt"
	ast "hover/Interpreter/ast"
	"hover/Interpreter/elaborator"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// USER-DEFINED FUNCTIONS
// ─────────────────────────────────────────────────────────────────────────────

// emitFunctions emits every user-defined function reachable from the entry
// file — both bare (entry file's own functions plus bare imports) and
// aliased (import "x.hvr" as Y; → Y.funcName).
func (g *generator) emitFunctions() {
	hasAliased := false
	for _, byName := range g.prog.AliasedFunctions {
		if len(byName) > 0 {
			hasAliased = true
			break
		}
	}
	if len(g.prog.Functions) == 0 && !hasAliased {
		return
	}
	g.raw(`// ── USER-DEFINED FUNCTIONS ───────────────────────────────────────────────────`)

	// Forward-declare every function before emitting any body. Without
	// this, a function whose body calls another user-defined function
	// (e.g. math.hvr's ln1p() calling ln(), log10() calling ln()) would
	// only compile if the callee happened to be emitted FIRST — but
	// g.prog.Functions is a Go map, whose iteration order is randomized
	// per run by design. This is not hypothetical: confirmed directly,
	// math.hvr's ln1p/log10 failed to compile with "'ln' was not
	// declared in this scope" on a real run where map iteration order
	// put them before ln() itself. C++ forward declarations (a one-line
	// prototype per function, in any order) are the standard fix for
	// exactly this — mirrors how a C/C++ header separates declarations
	// from definitions so call order never matters.
	for _, fn := range g.prog.Functions {
		g.emitForwardDecl(fn.Name, fn)
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			cName := mangle(alias + "." + fn.Name)
			g.emitForwardDecl(cName, fn)
		}
	}
	g.raw("")

	// Bare functions (entry file + bare imports) — C name == Hover name,
	// exactly as before.
	for _, fn := range g.prog.Functions {
		g.emitOneFunction(fn.Name, fn)
	}

	// Aliased functions (Motor.sineWave etc.) — C name is mangled to
	// "Alias_funcName" so two aliases can each have their own "sineWave"
	// without colliding as C identifiers. The internal Prefix used for
	// local-variable resolution inside the body is the same mangled name,
	// so parameter locals stay unique per (alias, function) pair too.
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			cName := mangle(alias + "." + fn.Name)
			g.emitOneFunction(cName, fn)
		}
	}
}

// emitForwardDecl emits a single C++ function prototype (signature only,
// no body, terminated with a semicolon) — see emitFunctions' comment for
// why every function needs one of these ahead of every body.
func (g *generator) emitForwardDecl(cName string, fn *ast.FuncDeclStatement) {
	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		paramType := hoverTypeToCType(p.Type)
		params = append(params, fmt.Sprintf("%s %s", paramType.String(), p.Name))
	}
	returnType := hoverTypeToCType(fn.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s);", returnType.String(), cName, strings.Join(params, ", ")))
}

// emitOneFunction emits a single function declaration under the given C
// name. cName is used both as the emitted C function's identifier and as
// the Prefix for resolving the function's own parameters as locals inside
// its body — this is what makes Motor_sineWave's "freq" parameter distinct
// from Aux_sineWave's "freq" parameter, even though both are named "freq"
// in their respective Hover source files.
//
// Parameters and the return type now use their real declared CType
// (int64_t, uint64_t, float, double) rather than a uniform double.
func (g *generator) emitOneFunction(cName string, fn *ast.FuncDeclStatement) {
	fnLogic := elaborator.LogicObject{
		Prefix: cName,
		Params: make(map[string]float64),
		Ports:  make(map[string]string),
	}

	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		paramType := hoverTypeToCType(p.Type)
		params = append(params, fmt.Sprintf("%s %s", paramType.String(), p.Name))
		// Mark as a local — resolveIdent will find it as cName.paramName,
		// which mangle() later turns into the C local cName_paramName.
		fnLogic.Ports[p.Name] = cName + "." + p.Name
	}

	returnType := hoverTypeToCType(fn.ReturnType)

	g.raw(fmt.Sprintf("static %s %s(%s) {", returnType.String(), cName, strings.Join(params, ", ")))
	g.push()

	// Declare local params as C variables so expressions can reference
	// them, each with its own declared type — these are plain function
	// arguments, no cast needed since the parameter's C++ type in the
	// signature above already matches.
	for _, p := range fn.Parameters {
		paramType := hoverTypeToCType(p.Type)
		g.line("%s %s_%s = %s;", paramType.String(), cName, p.Name, p.Name)
	}

	g.emitStmt(fn.Body, fnLogic, true)
	g.line("return %s; // fallthrough", formatTypedLiteral(0.0, returnType))
	g.pop()
	g.raw("}")
	g.raw("")
}
