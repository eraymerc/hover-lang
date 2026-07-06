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

	// Forward declarations (skip extern — header provides them).
	for _, fn := range g.prog.Functions {
		if fn.IsExtern {
			continue
		}
		g.emitForwardDecl(fn.Name, fn)
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			if fn.IsExtern {
				continue
			}
			g.emitForwardDecl(mangle(alias+"."+fn.Name), fn)
		}
	}
	g.raw("")

	// Bodies (skip extern — header provides them).
	for _, fn := range g.prog.Functions {
		if fn.IsExtern {
			continue
		}
		g.emitOneFunction(fn.Name, fn)
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			if fn.IsExtern {
				continue
			}
			g.emitOneFunction(mangle(alias+"."+fn.Name), fn)
		}
	}
}

// emitForwardDecl emits a single C++ function prototype. Parameters and the
// return type carry their full declared shape (arrays decay to pointers as
// parameters, exactly like C; an array return degrades to the element type).
func (g *generator) emitForwardDecl(cName string, fn *ast.FuncDeclStatement) {
	params := []string{"VM *vm"}
	for _, p := range fn.Parameters {
		params = append(params, hoverTypeOf(p.Type).cParamDecl(p.Name))
	}
	ret := hoverTypeOf(fn.ReturnType)
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
		params = append(params, hoverTypeOf(p.Type).cParamDecl(p.Name))
		fnLogic.Ports[p.Name] = cName + "." + p.Name
	}

	ret := hoverTypeOf(fn.ReturnType)
	g.raw(fmt.Sprintf("static %s %s(%s) {", ret.cReturnType(), cName, strings.Join(params, ", ")))
	g.push()

	for _, p := range fn.Parameters {
		localName := fmt.Sprintf("%s_%s", cName, p.Name)
		g.line("%s", hoverTypeOf(p.Type).cDecayedLocalDecl(localName, p.Name))
	}

	g.emitStmt(fn.Body, fnLogic, true)
	g.line("return %s; // fallthrough", formatTypedLiteral(0.0, ret.elem))
	g.pop()
	g.raw("}")
	g.raw("")
}
