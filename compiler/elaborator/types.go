package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"hover/compiler/token"
)

type PhysicalObject struct {
	Type         string
	Name         string
	Parameters   map[string]float64
	Nodes        []string
	CtrlSignal   string // Mangled logic signal driving this primitive (voltage_source / current_source)
	SenseElement string // Sensing branch name for CCCS / CCVS (e.g. "main.Vsense")

	// UserNamed distinguishes `R rsense<1m>()` (Name == "main.rsense") from an
	// unnamed primitive whose Name was synthesized positionally ("main.R_3").
	// Only user-named elements are offered as suggestions in I()/.save()
	// diagnostics — a synthesized name is an implementation detail that
	// nobody should be typing, and suggesting it would be actively bad advice.
	UserNamed bool

	// Line is the source line of the declaring statement, kept so post-flatten
	// validation (sense resolution, name collisions) can point at real source
	// instead of a flattened name that has no location of its own.
	Line int
}

type LogicObject struct {
	Source ast.Statement
	Reads  []string
	Writes []string

	// Execution context captured at elaboration time so the VM can resolve
	// identifiers without knowing the module hierarchy.
	Prefix string             // e.g. "main.generator"
	Params map[string]float64 // Static params evaluated at elaboration time
	Ports  map[string]string  // Local port name → mangled wire/signal name

	Domain token.Type
}

type ElaboratedProgram struct {
	Physicals  []PhysicalObject
	Logic      []LogicObject
	Directives []*ast.DirectiveStatement
	Symbols    map[string]ast.Type
	Functions  map[string]*ast.FuncDeclStatement // User-defined functions for runtime calls

	// AliasedFunctions holds functions from `import "x.hvr" as Y;` style
	// imports, reachable only as Y.funcName — never merged into Functions
	// above. Keyed first by alias, then by function name.
	AliasedFunctions map[string]map[string]*ast.FuncDeclStatement
	CIncludes        []string // importc headers, in discovery order (deduped at codegen)

}

// ImportedFile is one file's parsed program plus its own (non-transitive)
// import table. The entry file is included in this set like any other —
// it just happens to have FilePath == the path passed to Load().
type ImportedFile struct {
	FilePath string
	Program  *ast.Program
	// Imports lists this file's own `import` statements, already parsed
	// into ast.ImportStatement nodes (Path here is the raw source string,
	// e.g. "./foo/bar.hvr" — ResolvePath converts that to a key into Files).
	Imports []*ast.ImportStatement
}

type Elaborator struct {
	// scopes maps an absolute file path to that file's module namespace, and
	// declFile maps a module declaration back to the file that declared it.
	// Together they let flattenModule resolve a module body's references
	// against the importing file's OWN import list — see scope.go for why
	// that matters and what the previous entry-file-owns-everything model
	// broke.
	//
	// output.Functions / output.AliasedFunctions are deliberately NOT part of
	// this yet: function resolution is still entry-file-scoped (stage 2).
	scopes   map[string]*fileScope
	declFile map[*ast.ModuleDeclStatement]string

	// entryScope is the scope of the file compilation started from. It owns
	// 'main', and serves as the fallback for modules with no recorded origin
	// file (the single-file New() path).
	entryScope *fileScope

	output *ElaboratedProgram
	errors []string

	// expanding tracks which module declarations are currently being
	// flattened, by their resolved (possibly alias-qualified) name — e.g.
	// "A" or "Motor.PMSM". flattenModule pushes onto this before recursing
	// into a ModuleInstStatement's target and pops on return. If a name
	// is already present when about to be pushed again, that's a module
	// instantiation cycle: A instantiates B instantiates A, directly or
	// through any number of intermediate modules.
	//
	// This is independent of and unrelated to the loader's file-level
	// cycle detection — a module cycle can happen entirely within one
	// file, with no imports involved at all, and the loader's check
	// (which only looks at import statements) cannot see it.
	// Keyed by the resolved declaration rather than the reference spelling:
	// under per-file scoping the same module can be named differently from
	// different files (bare "LED" here, "L.LED" there), and a cycle is a
	// property of the declaration, not of how someone spelled it.
	expanding map[*ast.ModuleDeclStatement]bool

	// elementIndex maps a flattened element name ("main.motor.vsense") to its
	// index in output.Physicals. EVERY physical is registered here, user-named
	// or not, because both share a single namespace: the runtime keys its
	// branch_map by exactly these strings, so a hand-written `R R_3<...>` and
	// a synthesized "R_3" would otherwise silently collide there.
	//
	// It is also the resolution target for a CCCS/CCVS sense reference. It is
	// intentionally not exported on ElaboratedProgram — codegen rebuilds the
	// small subsets it needs from Physicals, mirroring physNodeSet.
	elementIndex map[string]int

	// pendingSense holds CCCS/CCVS sense references discovered while
	// flattening, resolved in a post-pass (resolveSenseElements in
	// elements.go). It cannot be resolved inline: the sensed voltage source is
	// frequently declared AFTER the controlled source in the same module body,
	// and that has to work — codegen emits register_netlist (all branches)
	// strictly before stamp_netlist (all resolve_branch calls), so the netlist
	// imposes no ordering constraint for this to inherit.
	pendingSense []senseRef
}

// senseRef is one unresolved CCCS/CCVS controlling-element reference.
type senseRef struct {
	physIdx int    // index into output.Physicals of the CCCS/CCVS itself
	target  string // fully-flattened candidate name, e.g. "main.vsense"
	raw     string // as written in source, for the error message
	line    int
}

// New builds an Elaborator from a single already-parsed program, with no
// import support. Existing callers that only ever had one file keep working
// unchanged.
func New(program *ast.Program) *Elaborator {
	e := newElaborator()
	// One file, no imports: a single scope holding its own declarations, which
	// is also the entry scope every module resolves against.
	sc := newFileScope("")
	for _, stmt := range program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok {
			sc.modules[m.Name] = m
			e.declFile[m] = ""
		}
	}
	e.scopes[""] = sc
	e.entryScope = sc

	e.registerFileDecls(program)
	return e
}

// newElaborator allocates the maps shared by both constructors. Kept in one
// place so a newly added field can't be initialised in one constructor and
// silently left nil in the other.
func newElaborator() *Elaborator {
	return &Elaborator{
		scopes:       make(map[string]*fileScope),
		declFile:     make(map[*ast.ModuleDeclStatement]string),
		expanding:    make(map[*ast.ModuleDeclStatement]bool),
		elementIndex: make(map[string]int),
		output: &ElaboratedProgram{
			Physicals:        []PhysicalObject{},
			Logic:            []LogicObject{},
			Symbols:          make(map[string]ast.Type),
			Functions:        make(map[string]*ast.FuncDeclStatement),
			AliasedFunctions: make(map[string]map[string]*ast.FuncDeclStatement),
		},
	}
}

// NewWithImports builds an Elaborator from the entry file plus every file
// it (non-transitively) imports. files maps an absolute file path to its
// ImportedFile record; entryPath identifies which one is the root.
//
// Resolution rules:
//   - Bare import (`import "x.hvr";`)        → x.hvr's modules/functions are
//     merged directly into the importing file's flat namespace. A name that
//     already exists (from the file itself, or from another bare import) is a
//     collision error.
//   - Aliased import (`import "x.hvr" as Y;`) → x.hvr's modules/functions
//     are reachable only as Y.Thing, kept in a separate per-alias map.
//
// Imports are non-transitive: if x.hvr itself imports y.hvr, the importer sees
// x.hvr's own declarations only, never y.hvr's.
//
// MODULES are scoped per file (scope.go): every file resolves module
// references against its own import list, so a library can declare its own
// dependencies and pick its own aliases. FUNCTIONS are still resolved from the
// entry file's imports alone — that's stage 2, and it additionally requires
// codegen changes, so the two registration paths below deliberately differ.
func NewWithImports(files map[string]*ImportedFile, entryPath string) (*Elaborator, error) {
	entry, ok := files[entryPath]
	if !ok {
		return nil, fmt.Errorf("entry file %q not found in loaded file set", entryPath)
	}

	e := newElaborator()

	// 1. Per-file module scopes, for every loaded file.
	if err := e.buildModuleScopes(files); err != nil {
		return nil, err
	}
	e.entryScope = e.scopes[entryPath]

	// 2. Register the entry file's own declarations first, so collision
	//    errors can correctly say "already declared in the entry file"
	//    rather than attributing the original declaration to an import.
	e.registerFileDecls(entry.Program)

	// 2. Resolve each of the entry file's own imports (non-transitive —
	//    we only ever look at entry.Imports, never recurse into another
	//    file's import list).
	for _, imp := range entry.Imports {
		resolvedPath := resolveImportPathFor(entryPath, imp.Path, imp.IsSystem)
		imported, ok := files[resolvedPath]
		if !ok {
			return nil, fmt.Errorf("line %d: import %q could not be resolved (looked for %s) — was it loaded?",
				imp.Token.Line, imp.Path, resolvedPath)
		}

		if imp.Alias != "" {
			if err := e.registerAliasedFile(imp.Alias, imported.Program); err != nil {
				return nil, fmt.Errorf("line %d: %w", imp.Token.Line, err)
			}
		} else {
			if err := e.mergeBareFile(imported.Program); err != nil {
				return nil, fmt.Errorf("line %d: %w", imp.Token.Line, err)
			}
		}
	}

	return e, nil
}

// registerFileDecls walks the ENTRY file's top-level statements and populates
// e.output.Functions / Directives / CIncludes.
//
// Modules are deliberately absent: they live in per-file scopes built by
// buildModuleScopes (scope.go), so registering them here too would create a
// second, entry-only namespace that could disagree with the scoped one.
// Directives are entry-only by design — an imported library has no business
// setting .tran or .save on its consumer.
func (e *Elaborator) registerFileDecls(program *ast.Program) {
	for _, stmt := range program.Statements {
		if d, ok := stmt.(*ast.DirectiveStatement); ok {
			e.output.Directives = append(e.output.Directives, d)
		}
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			e.output.Functions[f.Name] = f
		}
		if ic, ok := stmt.(*ast.ImportCStatement); ok {
			e.output.CIncludes = append(e.output.CIncludes, ic.Path)
		}
	}
}

// mergeBareFile merges a bare-imported file's FUNCTION declarations into
// e.output.Functions. Returns an error if any name already exists — bare
// imports share one flat namespace, so collisions must be caught rather than
// silently shadowed.
//
// Modules are handled by buildModuleScopes (scope.go), which applies the same
// collision rule per importing file rather than globally.
func (e *Elaborator) mergeBareFile(program *ast.Program) error {
	for _, stmt := range program.Statements {
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			if existing, exists := e.output.Functions[f.Name]; exists {
				if existing == f {
					continue // same declaration via diamond import — fine
				}
				if f.IsExtern && existing.IsExtern {
					continue // duplicate extern declaration — harmless
				}
				return fmt.Errorf("function '%s' declared at line %d collides with an existing function '%s' (already declared at line %d) — use an aliased import (`as Name`) to avoid this",
					f.Name, f.Line(), existing.Name, existing.Line())
			}
			e.output.Functions[f.Name] = f
		}
		if ic, ok := stmt.(*ast.ImportCStatement); ok {
			e.output.CIncludes = append(e.output.CIncludes, ic.Path)
		}
	}
	return nil
}

// registerAliasedFile registers an aliased import's FUNCTION declarations
// under the given alias, reachable as Alias.funcName. Each alias gets its own
// isolated map, so aliased imports never collide with the flat namespace or
// with each other.
//
// Modules are handled by buildModuleScopes (scope.go), per importing file.
func (e *Elaborator) registerAliasedFile(alias string, program *ast.Program) error {
	if e.output.AliasedFunctions[alias] == nil {
		e.output.AliasedFunctions[alias] = make(map[string]*ast.FuncDeclStatement)
	}

	for _, stmt := range program.Statements {
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			e.output.AliasedFunctions[alias][f.Name] = f
		}
		if ic, ok := stmt.(*ast.ImportCStatement); ok {
			e.output.CIncludes = append(e.output.CIncludes, ic.Path)
		}
	}
	return nil
}

// resolveQualifiedFunction looks up "Alias.funcName" or a plain "funcName".
//
// STAGE 2 NOTE: unlike module resolution, this is still ENTRY-FILE scoped —
// it consults one global table built from the entry file's imports, so a
// library calling a function it imported itself still won't resolve. Moving
// functions to fileScope (scope.go) additionally requires threading the
// declaring file through LogicObject into codegen, and inverting codegen's
// alias-based mangling so one declaration reached through two aliases emits a
// single C function. standard_library/electromechanical/pmsm.hvr will need
// `import <math/math.hvr>;` added when that lands — it calls sin/cos today
// without importing them.
func (e *Elaborator) resolveQualifiedFunction(name string) (*ast.FuncDeclStatement, bool) {
	alias, bare, isQualified := splitQualifiedName(name)
	if !isQualified {
		decl, ok := e.output.Functions[name]
		return decl, ok
	}
	byName, ok := e.output.AliasedFunctions[alias]
	if !ok {
		return nil, false
	}
	decl, ok := byName[bare]
	return decl, ok
}
