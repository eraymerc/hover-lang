package codegen

import (
	"fmt"
	"sort"

	ast "hover/compiler/ast"
)

// emitStructDefs emits a real C++ `struct Name { ...fields...; };` for every
// Hover struct declaration loaded, called from emitHeader right after
// includes, before state-var emission (so struct types are already defined
// by the time any variable/function declaration needs them).
//
// Emission order is dependency-first (a struct with a `Point` field must be
// emitted after `struct Point`), computed here rather than trusted from
// source/file order — g.prog.Structs is a flat map with no ordering of its
// own (see buildStructRegistry), and even file order wouldn't guarantee
// this across files. Semantic analysis's forward-declaration-only field
// rule (registerStructDecl) makes the dependency graph acyclic by
// construction, so a straightforward DFS-based topological emit suffices;
// the "currently emitting" guard below only exists to fail loudly instead
// of infinite-looping if that invariant is ever violated.
func (g *generator) emitStructDefs() {
	if len(g.prog.Structs) == 0 {
		return
	}
	g.raw(`// ── STRUCT TYPES ─────────────────────────────────────────────────────────────`)

	names := make([]string, 0, len(g.prog.Structs))
	for name := range g.prog.Structs {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic traversal start; real order comes from the DFS below

	emitted := map[string]bool{}
	emitting := map[string]bool{}
	var emit func(name string)
	emit = func(name string) {
		if emitted[name] {
			return
		}
		sd, ok := g.prog.Structs[name]
		if !ok {
			return // referenced but not declared — semantic analysis already rejects this
		}
		if emitting[name] {
			panic("struct '" + name + "' is part of a cyclic field reference — semantic analysis should have rejected this")
		}
		emitting[name] = true
		for _, f := range sd.Fields {
			if _, isStruct := g.prog.Structs[f.Type.Base]; isStruct {
				emit(f.Type.Base)
			}
		}
		emitting[name] = false

		g.raw(fmt.Sprintf("struct %s {", sd.Name))
		g.push()
		for _, f := range sd.Fields {
			ht := g.hoverTypeOf(f.Type)
			g.line("%s;", ht.cVarDecl(f.Name))
		}
		g.pop()
		g.raw("};")

		emitted[name] = true
	}

	for _, name := range names {
		emit(name)
	}
	g.raw(``)
}

// structFieldType looks up a struct field's declared ast.Type by name.
// Small helper shared by the struct-literal emission paths (expressions.go,
// main_emit.go, statements.go) so each doesn't repeat the linear field
// search.
func (g *generator) structFieldType(structName, fieldName string) (ast.Type, bool) {
	sd, ok := g.prog.Structs[structName]
	if !ok {
		return ast.Type{}, false
	}
	for _, f := range sd.Fields {
		if f.Name == fieldName {
			return f.Type, true
		}
	}
	return ast.Type{}, false
}
