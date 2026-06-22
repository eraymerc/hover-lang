package codegen

import (
	ast "hover/Interpreter/ast"
	"hover/Interpreter/elaborator"
)

// ─────────────────────────────────────────────────────────────────────────────
// COLLECTORS
// These walk the elaborated program to build sets/maps used by the emit*
// functions elsewhere in this package. They never write to g.sb — pure
// read-only passes over g.prog.
// ─────────────────────────────────────────────────────────────────────────────

// collectStateVars returns all mangled names that are declared as 'state'
// across all LogicObjects, deduped, mapped to their initial value.
func (g *generator) collectStateVars() map[string]float64 {
	seen := make(map[string]float64)
	for _, logic := range g.prog.Logic {
		decl, ok := logic.Source.(*ast.LocalDeclStatement)
		if !ok || !decl.IsState {
			continue
		}
		for _, d := range decl.Decls {
			mangled := mangle(logic.Prefix + "." + d.Name)
			initVal := 0.0
			if d.Value != nil {
				if num, ok := d.Value.(*ast.NumberExpression); ok {
					initVal = elaborator.ParseEngineering(num.Value)
				}
			}
			seen[mangled] = initVal
		}
	}
	return seen
}

// collectStateVarDottedNames returns, for every mangled 'state' variable
// name collectStateVars would produce, the original dotted Hover name it
// came from (e.g. "main_ct_counter" -> "main.ct.counter"). This exists
// because mangle() (names.go) is a one-way transform — it replaces '.'
// with '_', which is NOT safely invertible in general, since Hover
// identifiers can themselves contain underscores (e.g. "ctrl_out"). A
// blind "_" -> "." reverse mapping would corrupt such names. Tracking the
// real dotted name here, at the same point mangle() is first applied,
// avoids ever needing to invert it.
//
// Used by emitStateVarSnapshot (phases.go) as the vm->values map key for
// state save/restore — that key must match exactly what phase_log already
// uses for the same variable (also the unmangled dotted form), or the ZCD
// snapshot/restore mechanism and the .save() logging mechanism would
// silently disagree about a variable's identity.
func (g *generator) collectStateVarDottedNames() map[string]string {
	dotted := make(map[string]string)
	for _, logic := range g.prog.Logic {
		decl, ok := logic.Source.(*ast.LocalDeclStatement)
		if !ok || !decl.IsState {
			continue
		}
		for _, d := range decl.Decls {
			dottedName := logic.Prefix + "." + d.Name
			mangled := mangle(dottedName)
			dotted[mangled] = dottedName
		}
	}
	return dotted
}

// collectAllVars collects ALL non-wire variable names across all logic
// blocks — both state and non-state locals. All of them become file-scope
// statics so they stay visible across phase-function boundaries (each
// phase is its own C function, so a Hover local declared in one statement
// must still be readable from a later statement even though the elaborator
// split them into separate LogicObjects).
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
			// Bare assignments to a name with no prior LocalDeclStatement
			// must still get a global declared for them — this is the
			// case for idt()/ddt()'s injected hidden-node assignments
			// (elaborator/macros.go), which write to a fresh identifier
			// that's never wrapped in a LocalDeclStatement at all. Without
			// this case, collectAllVars never sees that name, so
			// emitStateVars never declares its file-scope global, and the
			// generated C++ fails to compile with "was not declared in
			// this scope" the moment idt()/ddt() is actually exercised
			// (confirmed by direct testing — this bug pre-existed for
			// idt() too, just never surfaced because no prior test
			// exercised it standalone).
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
// Maps every mangled variable name to its declared CType, built once and
// cached on the generator (see generator.typeTable in generator.go). This
// is the symbol table the rest of codegen consults to know what C++ type
// a variable was declared with, since storage is no longer uniformly
// double — emitExpr needs this for every identifier it resolves, and
// emitStmt needs it to decide whether an assignment requires an explicit
// cast.
//
// Walks the exact same statement shapes as collectAllVars (LocalDeclStatement
// inside Block/If/While), but records the declared CType alongside the name
// instead of just the name. The two collectors are kept separate rather than
// merged into one, since collectAllVars's plain []string return is still
// the simplest shape for callers that only need ordered names (emitStateVars'
// declaration-order iteration) without also needing the type.
func (g *generator) collectVarTypes() map[string]CType {
	types := make(map[string]CType)

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
			ctype := hoverTypeToCType(s.Type)
			for _, d := range s.Decls {
				mangled := resolveWrite(d.Name, logic)
				types[mangled] = ctype
			}
		case *ast.AssignmentStatement:
			// Mirrors collectAllVars' AssignmentStatement case (see that
			// comment for the full idt()/ddt() rationale). A bare
			// assignment with no declaration has no explicit Hover type
			// to read, but every such case in practice (idt/ddt hidden
			// nodes, and any other bare-assigned name) is a plain
			// physical quantity — CDouble is correct here, and typeOf's
			// existing CDouble fallback would produce the same result if
			// this case were omitted, so this is a correctness/clarity
			// improvement rather than a behavior change.
			if id, ok := s.Left.(*ast.IdentifierExpression); ok {
				mangled := resolveWrite(id.Value, logic)
				if _, exists := types[mangled]; !exists {
					types[mangled] = CDouble
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

	// Function parameters also need a CType — they're locals inside their
	// own function body, resolved the same way (resolveIdent → mangled
	// cName.paramName → mangle()), but they never appear in g.prog.Logic
	// since they're not LogicObjects. Walk g.prog.Functions and
	// g.prog.AliasedFunctions to add them too.
	for _, fn := range g.prog.Functions {
		for _, p := range fn.Parameters {
			mangled := mangle(fn.Name + "." + p.Name)
			types[mangled] = hoverTypeToCType(p.Type)
		}
	}
	for alias, byName := range g.prog.AliasedFunctions {
		for _, fn := range byName {
			cName := mangle(alias + "." + fn.Name)
			for _, p := range fn.Parameters {
				mangled := mangle(cName + "." + p.Name)
				types[mangled] = hoverTypeToCType(p.Type)
			}
		}
	}

	return types
}

// typeOf looks up the CType for a mangled variable name, defaulting to
// CDouble if the name isn't found (e.g. a name resolveIdent produced for
// something not tracked by collectVarTypes, such as a static param literal
// — those never reach typeOf since resolveIdent returns a literal string
// for them directly, never an identifier name to look up).
func (g *generator) typeOf(mangledName string) CType {
	if g.typeTable == nil {
		g.typeTable = g.collectVarTypes()
	}
	if t, ok := g.typeTable[mangledName]; ok {
		return t
	}
	return CDouble
}
