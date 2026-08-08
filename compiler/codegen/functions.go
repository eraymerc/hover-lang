package codegen

import (
	"fmt"
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// USER-DEFINED FUNCTIONS
//
// `extern func` declarations (FuncDeclStatement.IsExtern) are skipped here:
// their definition lives in a C/C++ header pulled in by `importc`, so Hover
// must NOT emit a forward declaration or a body for them. They are still
// present in g.prog.Functions so calls can resolve their signature — see
// emitUserFunctionCall, which emits them as raw C calls (no vm, no mangling).
// ─────────────────────────────────────────────────────────────────────────────

func (g *generator) emitFunctions() {
	if len(g.prog.Functions) == 0 {
		return
	}
	g.raw(`// ── USER-DEFINED FUNCTIONS ───────────────────────────────────────────────────`)

	// One C function per DECLARATION, under the name the elaborator assigned
	// it (FunctionInfo.CName) — not per call spelling. A function imported
	// bare by one file and under an alias by another is still one declaration
	// and must not be emitted twice.
	//
	// Iteration order is the elaborator's deterministic one (declaring file
	// path, then source order), so sim.cpp is byte-reproducible.
	for _, fn := range g.prog.Functions {
		if fn.Decl.IsExtern {
			continue
		}
		g.emitForwardDecl(fn.CName, fn.Decl)
	}
	g.raw("")

	for _, fn := range g.prog.Functions {
		if fn.Decl.IsExtern {
			continue
		}
		g.emitOneFunction(fn)
	}
}

// emitForwardDecl emits a single C++ function prototype. Parameters and the
// return type carry their full declared shape (arrays decay to pointers as
// parameters, exactly like C; an array return degrades to the element type).
func (g *generator) emitForwardDecl(cName string, fn *ast.FuncDeclStatement) {
	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		params = append(params, g.hoverTypeOf(p.Type).cParamDecl(p.Name))
	}
	ret := g.hoverTypeOf(fn.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s);", ret.cReturnType(), cName, strings.Join(params, ", ")))
}

// emitOneFunction emits a single function under its assigned C name, which is
// both the emitted identifier and the Prefix used to resolve the function's
// own parameters as locals inside its body.
//
// The synthesized LogicObject carries fn.File, so calls made from inside this
// body resolve against the imports of the file that DECLARED it — the same
// rule module bodies follow.
func (g *generator) emitOneFunction(fn *elaborator.FunctionInfo) {
	decl := fn.Decl
	fnLogic := elaborator.LogicObject{
		Prefix: fn.CName,
		Params: make(map[string]float64),
		Ports:  make(map[string]string),
		File:   fn.File,
	}

	params := []string{"VM *vm"}
	for _, p := range decl.Parameters {
		params = append(params, g.hoverTypeOf(p.Type).cParamDecl(p.Name))
		fnLogic.Ports[p.Name] = fn.CName + "." + p.Name
	}

	ret := g.hoverTypeOf(decl.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s) {", ret.cReturnType(), fn.CName, strings.Join(params, ", ")))
	g.push()

	for _, p := range decl.Parameters {
		localName := mangle(fn.CName + "." + p.Name)
		g.line("%s", g.hoverTypeOf(p.Type).cDecayedLocalDecl(localName, p.Name))
	}

	g.emitStmt(decl.Body, fnLogic, true)
	g.line("return %s; // fallthrough", ret.zeroValue())
	g.pop()
	g.raw("}")
	g.raw("")
}
