package codegen

import (
	ast "hover/compiler/ast"
	"hover/compiler/elaborator"
)

// ─────────────────────────────────────────────────────────────────────────────
// COLLECTORS
// These walk the elaborated program to build sets/maps used by the emit*
// functions elsewhere in this package. They never write to g.sb — pure
// read-only passes over g.prog.
// ─────────────────────────────────────────────────────────────────────────────

// collectStateInits maps every mangled 'state' variable to its initializer
// EXPRESSION (nil if the declaration had none). Presence in the map means
// "this name is a state var", so callers use the two-value lookup
// `init, isState := m[name]` even when init is nil.
//
// This replaces the old float64-valued collectStateVars: an init value can be
// an array literal ({1,2}) or a scalar-fill (1e-6), neither of which fits in a
// single float64 — that limitation is exactly what left `state double[2] arr`
// initialized to 0. Snapshot code that only needs the set of state names just
// ranges over this map's keys.
type stateInit struct {
	value ast.Expression
	logic elaborator.LogicObject
}

func (g *generator) collectStateInits() map[string]stateInit {
	inits := make(map[string]stateInit)
	for _, logic := range g.prog.Logic {
		decl, ok := logic.Source.(*ast.LocalDeclStatement)
		if !ok || !decl.IsState {
			continue
		}
		for _, d := range decl.Decls {
			inits[mangle(logic.Prefix+"."+d.Name)] = stateInit{value: d.Value, logic: logic}
		}
	}
	return inits
}

// collectStateVarDottedNames returns, for every mangled 'state' variable name,
// the original dotted Hover name it came from (e.g. "main_ct_counter" ->
// "main.ct.counter"). mangle() (names.go) is a one-way transform — it replaces
// '.' with '_', which is not safely invertible since Hover identifiers can
// themselves contain underscores. Tracking the dotted name here, at the point
// mangle() is first applied, avoids ever needing to invert it.
//
// Used by emitStateVarSnapshot (phases.go) as the vm->values map key for state
// save/restore — that key must match exactly what phase_log uses for the same
// variable, or ZCD snapshot/restore and .save() logging would disagree about a
// variable's identity.
func (g *generator) collectStateVarDottedNames() map[string]string {
	dotted := make(map[string]string)
	for _, logic := range g.prog.Logic {
		decl, ok := logic.Source.(*ast.LocalDeclStatement)
		if !ok || !decl.IsState {
			continue
		}
		for _, d := range decl.Decls {
			dottedName := logic.Prefix + "." + d.Name
			dotted[mangle(dottedName)] = dottedName
		}
	}
	return dotted
}

// collectAllVars collects ALL non-wire variable names across all logic blocks
// — both state and non-state locals. All become file-scope statics so they
// stay visible across phase-function boundaries (each phase is its own C
// function, so a Hover local declared in one statement must still be readable
// from a later statement even though the elaborator split them apart).
func (g *generator) collectAllVars() []string {
	seen := make(map[string]bool)
	var vars []string

	var walk func(stmt ast.Statement, logic elaborator.LogicObject)
	walk = func(stmt ast.Statement, logic elaborator.LogicObject) {
		if stmt == nil {
			return
		}
		switch s := stmt.(type) {
		case *ast.LocalDeclStatement:
			if s.Type == "wire" {
				return
			}
			for _, d := range s.Decls {
				mangled := resolveWrite(d.Name, logic)
				if !seen[mangled] {
					seen[mangled] = true
					vars = append(vars, mangled)
				}
			}
		case *ast.AssignmentStatement:
			// Bare assignments with no prior LocalDeclStatement must still get
			// a global declared (idt()/ddt()'s injected hidden-node writes —
			// elaborator/macros.go — target a fresh identifier that is never
			// wrapped in a LocalDeclStatement). Without this they'd never be
			// declared and the generated C++ would fail to compile.
			if id, ok := s.Left.(*ast.IdentifierExpression); ok {
				mangled := resolveWrite(id.Value, logic)
				if !seen[mangled] {
					seen[mangled] = true
					vars = append(vars, mangled)
				}
			}
		case *ast.BlockStatement:
			for _, child := range s.Body {
				walk(child, logic)
			}
		case *ast.IfStatement:
			walk(s.Consequence, logic)
			for _, alt := range s.Alternatives {
				walk(alt.Body, logic)
			}
			if s.Alternative != nil {
				walk(s.Alternative, logic)
			}
		case *ast.WhileStatement:
			walk(s.Body, logic)
		}
	}

	for _, logic := range g.prog.Logic {
		walk(logic.Source, logic)
	}
	return vars
}

// ─────────────────────────────────────────────────────────────────────────────
// TYPE TABLE
//
// collectVarTypes maps every mangled variable name to its full hoverType —
// element CType plus pointer/array shape (see declarator.go). This single
// collector supersedes the former pair (an element-only map[string]CType and a
// raw-string map[string]string) which walked identical AST shapes for two
// views of the same fact: the hoverType carries both, so callers that want the
// element type read .elem and callers emitting declarations use cVarDecl /
// isArray / etc. directly.
// ─────────────────────────────────────────────────────────────────────────────

func (g *generator) collectVarTypes() map[string]hoverType {
	types := make(map[string]hoverType)

	var walk func(stmt ast.Statement, logic elaborator.LogicObject)
	walk = func(stmt ast.Statement, logic elaborator.LogicObject) {
		if stmt == nil {
			return
		}
		switch s := stmt.(type) {
		case *ast.LocalDeclStatement:
			if s.Type == "wire" {
				return
			}
			ht := parseHoverType(s.Type)
			for _, d := range s.Decls {
				types[resolveWrite(d.Name, logic)] = ht
			}
		case *ast.AssignmentStatement:
			// Bare-assigned names (idt/ddt hidden nodes) have no declared type;
			// they are always plain physical scalars — default to double.
			if id, ok := s.Left.(*ast.IdentifierExpression); ok {
				mangled := resolveWrite(id.Value, logic)
				if _, exists := types[mangled]; !exists {
					types[mangled] = parseHoverType("double")
				}
			}
		case *ast.BlockStatement:
			for _, child := range s.Body {
				walk(child, logic)
			}
		case *ast.IfStatement:
			walk(s.Consequence, logic)
			for _, alt := range s.Alternatives {
				walk(alt.Body, logic)
			}
			if s.Alternative != nil {
				walk(s.Alternative, logic)
			}
		case *ast.WhileStatement:
			walk(s.Body, logic)
		}
	}

	for _, logic := range g.prog.Logic {
		walk(logic.Source, logic)
	}

	// Function parameters are locals inside their own function body, resolved
	// as cName.paramName -> mangle(); they never appear in g.prog.Logic, so
	// walk g.prog.Functions and g.prog.AliasedFunctions to record them too.
	for _, fn := range g.prog.Functions {
		for _, p := range fn.Parameters {
			types[mangle(fn.Name+"."+p.Name)] = parseHoverType(p.Type)
		}
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			cName := mangle(alias + "." + fn.Name)
			for _, p := range fn.Parameters {
				types[mangle(cName+"."+p.Name)] = parseHoverType(p.Type)
			}
		}
	}

	return types
}

// typeOf looks up the full hoverType for a mangled variable name, defaulting
// to a plain double if the name isn't tracked (e.g. a static-param literal,
// which resolveIdent emits directly and never routes through here). Read .elem
// for the element CType.
func (g *generator) typeOf(mangledName string) hoverType {
	if g.typeTable == nil {
		g.typeTable = g.collectVarTypes()
	}
	if t, ok := g.typeTable[mangledName]; ok {
		return t
	}
	return parseHoverType("double")
}
