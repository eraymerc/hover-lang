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
// file — both bare (entry file's own functions plus bare imports) and aliased
// (import "x.hvr" as Y; → Y.funcName).
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

	// Forward-declare every function before emitting any body, so a function
	// whose body calls another resolves regardless of Go map iteration order.
	for _, fn := range g.prog.Functions {
		g.emitForwardDecl(fn.Name, fn)
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			g.emitForwardDecl(mangle(alias+"."+fn.Name), fn)
		}
	}
	g.raw("")

	for _, fn := range g.prog.Functions {
		g.emitOneFunction(fn.Name, fn)
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			g.emitOneFunction(mangle(alias+"."+fn.Name), fn)
		}
	}
}

// emitForwardDecl emits a single C++ function prototype. Parameters and the
// return type carry their full declared shape (arrays decay to pointers as
// parameters, exactly like C; an array return degrades to the element type
// since C/C++ can't return arrays).
func (g *generator) emitForwardDecl(cName string, fn *ast.FuncDeclStatement) {
	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		params = append(params, parseHoverType(p.Type).cParamDecl(p.Name))
	}
	ret := parseHoverType(fn.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s);", ret.cReturnType(), cName, strings.Join(params, ", ")))
}

// emitOneFunction emits a single function under the given C name. cName is both
// the emitted identifier and the Prefix used to resolve the function's own
// parameters as locals inside its body.
func (g *generator) emitOneFunction(cName string, fn *ast.FuncDeclStatement) {
	fnLogic := elaborator.LogicObject{
		Prefix: cName,
		Params: make(map[string]float64),
		Ports:  make(map[string]string),
	}

	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		params = append(params, parseHoverType(p.Type).cParamDecl(p.Name))
		fnLogic.Ports[p.Name] = cName + "." + p.Name
	}

	ret := parseHoverType(fn.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s) {", ret.cReturnType(), cName, strings.Join(params, ", ")))
	g.push()

	// Renamed local alias per parameter. Scalars copy; arrays/pointers alias
	// the caller's storage (array params decay to pointers, C-style), so the
	// body can index and mutate them and the caller sees the change.
	for _, p := range fn.Parameters {
		localName := fmt.Sprintf("%s_%s", cName, p.Name)
		g.line("%s", parseHoverType(p.Type).cDecayedLocalDecl(localName, p.Name))
	}

	g.emitStmt(fn.Body, fnLogic, true)
	g.line("return %s; // fallthrough", formatTypedLiteral(0.0, ret.elem))
	g.pop()
	g.raw("}")
	g.raw("")
}
