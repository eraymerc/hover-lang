package elaborator

import (
	"fmt"
	"hover/Interpreter/ast"
	"hover/Interpreter/token"
)

type PhysicalObject struct {
	Type         string
	Name         string
	Parameters   map[string]float64
	Nodes        []string
	CtrlSignal   string // Mangled logic signal driving this primitive (voltage_source / current_source)
	SenseElement string // Sensing branch name for CCCS / CCVS (e.g. "main.Vsense")
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
	Symbols    map[string]string
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
	// modules and output.Functions hold the ENTRY FILE's own declarations
	// plus everything merged in from bare (non-aliased) imports. This is
	// the flat, non-namespaced lookup that flattenModule/evaluate already
	// use today — unchanged in shape, just populated from more than one
	// file now.
	modules map[string]*ast.ModuleDeclStatement

	// aliasedModules holds modules from imports that used `as Name` —
	// reachable only as Name.Thing, never merged into the bare namespace
	// above. e.output.AliasedFunctions plays the equivalent role for
	// functions and is exposed directly on ElaboratedProgram since codegen
	// needs to resolve qualified function calls after elaboration finishes.
	aliasedModules map[string]map[string]*ast.ModuleDeclStatement

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
	expanding map[string]bool
}

// New builds an Elaborator from a single already-parsed program, with no
// import support. Existing callers that only ever had one file keep working
// unchanged.
func New(program *ast.Program) *Elaborator {
	e := &Elaborator{
		modules:        make(map[string]*ast.ModuleDeclStatement),
		aliasedModules: make(map[string]map[string]*ast.ModuleDeclStatement),
		expanding:      make(map[string]bool),
		output: &ElaboratedProgram{
			Physicals:        []PhysicalObject{},
			Logic:            []LogicObject{},
			Symbols:          make(map[string]string),
			Functions:        make(map[string]*ast.FuncDeclStatement),
			AliasedFunctions: make(map[string]map[string]*ast.FuncDeclStatement),
		},
	}
	e.registerFileDecls(program)
	return e
}

// NewWithImports builds an Elaborator from the entry file plus every file
// it (non-transitively) imports. files maps an absolute file path to its
// ImportedFile record; entryPath identifies which one is the root.
//
// Resolution rules:
//   - Bare import (`import "x.hvr";`)        → x.hvr's modules/functions are
//     merged directly into the entry file's flat namespace. A name that
//     already exists (from the entry file itself, or from another bare
//     import) is a collision error.
//   - Aliased import (`import "x.hvr" as Y;`) → x.hvr's modules/functions
//     are reachable only as Y.Thing, kept in a separate per-alias map.
//
// Imports are non-transitive: only the entry file's own `import` statements
// are consulted. If x.hvr itself imports y.hvr, that import is irrelevant
// here — the entry file never sees y.hvr unless it imports it directly.
func NewWithImports(files map[string]*ImportedFile, entryPath string) (*Elaborator, error) {
	entry, ok := files[entryPath]
	if !ok {
		return nil, fmt.Errorf("entry file %q not found in loaded file set", entryPath)
	}

	e := &Elaborator{
		modules:        make(map[string]*ast.ModuleDeclStatement),
		aliasedModules: make(map[string]map[string]*ast.ModuleDeclStatement),
		expanding:      make(map[string]bool),
		output: &ElaboratedProgram{
			Physicals:        []PhysicalObject{},
			Logic:            []LogicObject{},
			Symbols:          make(map[string]string),
			Functions:        make(map[string]*ast.FuncDeclStatement),
			AliasedFunctions: make(map[string]map[string]*ast.FuncDeclStatement),
		},
	}

	// 1. Register the entry file's own declarations first, so collision
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

// registerFileDecls walks one file's top-level statements and populates
// e.modules / e.output.Functions / e.output.Directives directly — this is
// exactly what the old New() did inline, factored out so both constructors
// share it.
func (e *Elaborator) registerFileDecls(program *ast.Program) {
	for _, stmt := range program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok {
			e.modules[m.Name] = m
		}
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

// mergeBareFile merges program's module/function declarations directly
// into e.modules / e.output.Functions. Returns an error if any name
// already exists — bare imports share one flat namespace with the entry
// file, so collisions must be caught rather than silently shadowed.
func (e *Elaborator) mergeBareFile(program *ast.Program) error {
	for _, stmt := range program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok {
			if existing, exists := e.modules[m.Name]; exists {
				if existing == m {
					continue // same declaration via two import paths (diamond) — fine
				}
				return fmt.Errorf("module '%s' declared at line %d collides with an existing module '%s' (already declared at line %d) — use an aliased import (`as Name`) to avoid this",
					m.Name, m.Line(), existing.Name, existing.Line())
			}
			e.modules[m.Name] = m
		}
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

// registerAliasedFile registers program's module/function declarations
// under the given alias, reachable as Alias.Thing. Unlike bare imports,
// aliased imports never collide with the entry namespace or with each
// other — each alias gets its own isolated map. The only error case is a
// file importing the same alias twice with different targets, which
// mergeBareFile-style collision logic doesn't apply to since aliases are
// keyed independently per file; that case is left as future work if it
// becomes a problem in practice.
func (e *Elaborator) registerAliasedFile(alias string, program *ast.Program) error {
	if e.aliasedModules[alias] == nil {
		e.aliasedModules[alias] = make(map[string]*ast.ModuleDeclStatement)
	}
	if e.output.AliasedFunctions[alias] == nil {
		e.output.AliasedFunctions[alias] = make(map[string]*ast.FuncDeclStatement)
	}

	for _, stmt := range program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok {
			e.aliasedModules[alias][m.Name] = m
		}
		if f, ok := stmt.(*ast.FuncDeclStatement); ok {
			e.output.AliasedFunctions[alias][f.Name] = f
		}
		if ic, ok := stmt.(*ast.ImportCStatement); ok {
			e.output.CIncludes = append(e.output.CIncludes, ic.Path)
		}
	}
	return nil
}

// resolveQualifiedModule looks up a possibly-aliased module name like
// "Motor.PMSM". If name contains no dot, it's a plain lookup in the flat
// (entry + bare-imports) namespace — e.modules. If it contains a dot, the
// part before the first dot is treated as an import alias.
//
// Returns (decl, ok). ok is false if the name doesn't resolve at all
// (unknown alias, or unknown name within a known alias).
func (e *Elaborator) resolveQualifiedModule(name string) (*ast.ModuleDeclStatement, bool) {
	alias, bare, isQualified := splitQualifiedName(name)
	if !isQualified {
		decl, ok := e.modules[name]
		return decl, ok
	}
	byName, ok := e.aliasedModules[alias]
	if !ok {
		return nil, false
	}
	decl, ok := byName[bare]
	return decl, ok
}

// resolveQualifiedFunction is the function-call equivalent of
// resolveQualifiedModule — looks up "Alias.funcName" or a plain "funcName".
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
