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

	// File is the path of the source file that declared the module this
	// statement came from. Codegen resolves function calls in this statement
	// against ElaboratedProgram.FuncScopes[File], so a library's module body
	// sees the library's imports rather than the consumer's.
	File string

	Domain token.Type
}

// FunctionInfo is one function declaration's compilation identity: the
// declaration itself, the single C identifier it will be emitted under, and
// the file that declared it.
//
// The C name belongs to the DECLARATION, not to any particular call site.
// That inversion is what makes per-file function scoping work: the same
// function reached bare from one file and as `M.fabs` from another is one
// declaration, so it must emit exactly one C function. The previous scheme
// derived the C name from the caller's spelling (mangle("M.fabs")), which
// would emit a second, identical copy per alias.
type FunctionInfo struct {
	Decl  *ast.FuncDeclStatement
	CName string // unique C identifier; for extern funcs, the raw header name
	File  string // declaring file, so calls inside its body resolve correctly
}

type ElaboratedProgram struct {
	Physicals  []PhysicalObject
	Logic      []LogicObject
	Directives []*ast.DirectiveStatement
	Symbols    map[string]ast.Type

	// Functions lists every function declaration in the loaded file set, in a
	// deterministic order (by declaring file path, then source order). Codegen
	// emits one C function per entry. Note this is every LOADED declaration,
	// not every REACHABLE one — an unused helper in an imported library is
	// emitted and left for the C++ compiler to discard, exactly as before.
	Functions []*FunctionInfo

	// FuncScopes is the per-file function namespace: file path → the call
	// spellings visible in that file → the declaration each resolves to. Bare
	// imports contribute plain names ("fabs"); aliased imports contribute
	// dotted ones ("M.fabs"). A file's own declarations are always present.
	//
	// This is the function-side counterpart of scope.go's per-file module
	// scopes, and it resolves the same way: a call inside a module body is
	// looked up in the scope of the file that DECLARED that module, never the
	// entry file's.
	FuncScopes map[string]map[string]*FunctionInfo

	// EntryFile is the path compilation started from — the fallback scope for
	// a LogicObject with no recorded declaring file (the single-file New()
	// path, where it is "").
	EntryFile string

	CIncludes []string // importc headers, in discovery order (deduped at codegen)

	// MainParams / MainPorts are the entry module's own `<>` static args
	// (name → its resolved initial value) and `()` logic args (name → the
	// mangled dotted signal it was bootstrapped to, always "main.<name>").
	// Recorded here by Elaborate() because they are the authoritative
	// answer to "what is main's interface" — the --hovercraft C ABI
	// (HVR_set_param_* / HVR_set_input_*) is generated directly from them.
	//
	// Codegen used to recover this by scanning Logic for a block whose
	// Prefix == "main", which silently produced the WRONG interface for a
	// structural main (one whose body only wires submodules together and
	// so contributes no LogicObject of its own): the scan fell through to
	// an arbitrary submodule's block and exported that submodule's ports
	// and params as if they were main's. Both maps are always non-nil,
	// and empty is a meaningful answer — main has no such args.
	MainParams map[string]float64
	MainPorts  map[string]string

	// Structs is every struct type declared across the loaded file set,
	// keyed by its bare declared name — a single flat namespace, not scoped
	// per file (see buildStructRegistry for why that diverges from
	// FuncScopes/module scoping).
	Structs map[string]*ast.StructDeclStatement
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
	// Functions are scoped the same way — see buildFunctionScopes in scope.go,
	// which fills output.FuncScopes from the same per-file import lists.
	scopes   map[string]*fileScope
	declFile map[*ast.ModuleDeclStatement]string

	// structFile maps a struct declaration back to the file that declared
	// it, used only to point a cross-file name-collision error at both
	// locations (buildStructRegistry) — the struct-decl counterpart of
	// declFile.
	structFile map[*ast.StructDeclStatement]string

	// entryScope is the scope of the file compilation started from. It owns
	// 'main', and serves as the fallback for modules with no recorded origin
	// file (the single-file New() path). entryFile is its path, used as the
	// same fallback on LogicObject.File.
	entryScope *fileScope
	entryFile  string

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
	// is also the entry scope every module resolves against. Its path key is ""
	// — the same key LogicObject.File carries on this path, and the same one
	// EntryFile points at, so function lookups land in it.
	sc := newFileScope("")
	for _, stmt := range program.Statements {
		if m, ok := stmt.(*ast.ModuleDeclStatement); ok {
			sc.modules[m.Name] = m
			e.declFile[m] = ""
		}
	}
	e.scopes[""] = sc
	e.entryScope = sc
	e.entryFile = ""
	e.output.EntryFile = ""

	files := map[string]*ImportedFile{"": {FilePath: "", Program: program}}
	// Cannot fail: one file with no imports has nothing to collide with and no
	// import path to resolve.
	_ = e.buildFunctionScopes(files)
	_ = e.buildStructRegistry(files)
	e.collectEntryDirectives(program)
	return e
}

// newElaborator allocates the maps shared by both constructors. Kept in one
// place so a newly added field can't be initialised in one constructor and
// silently left nil in the other.
func newElaborator() *Elaborator {
	return &Elaborator{
		scopes:       make(map[string]*fileScope),
		declFile:     make(map[*ast.ModuleDeclStatement]string),
		structFile:   make(map[*ast.StructDeclStatement]string),
		expanding:    make(map[*ast.ModuleDeclStatement]bool),
		elementIndex: make(map[string]int),
		output: &ElaboratedProgram{
			Physicals:  []PhysicalObject{},
			Logic:      []LogicObject{},
			Symbols:    make(map[string]ast.Type),
			FuncScopes: make(map[string]map[string]*FunctionInfo),
			MainParams: make(map[string]float64),
			MainPorts:  make(map[string]string),
			Structs:    make(map[string]*ast.StructDeclStatement),
		},
	}
}

// NewWithImports builds an Elaborator from the entry file plus every file
// it (non-transitively) imports. files maps an absolute file path to its
// ImportedFile record; entryPath identifies which one is the root.
//
// Resolution rules, applied identically to modules and functions:
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
// Both namespaces are scoped PER FILE: every file resolves references against
// its own import list, so a library can declare its own dependencies and pick
// its own aliases without the consumer having to repeat them.
func NewWithImports(files map[string]*ImportedFile, entryPath string) (*Elaborator, error) {
	entry, ok := files[entryPath]
	if !ok {
		return nil, fmt.Errorf("entry file %q not found in loaded file set", entryPath)
	}

	e := newElaborator()
	e.entryFile = entryPath
	e.output.EntryFile = entryPath

	// 1. Per-file module scopes, for every loaded file.
	if err := e.buildModuleScopes(files); err != nil {
		return nil, err
	}
	e.entryScope = e.scopes[entryPath]

	// 2. Per-file function scopes, likewise (scope.go).
	if err := e.buildFunctionScopes(files); err != nil {
		return nil, err
	}

	// 2b. Struct type registry — flat, not per-file (see buildStructRegistry).
	if err := e.buildStructRegistry(files); err != nil {
		return nil, err
	}

	// 3. Directives come from the entry file alone, by design — an imported
	//    library has no business setting .tran or .save on its consumer.
	e.collectEntryDirectives(entry.Program)

	return e, nil
}

// collectEntryDirectives records the entry file's simulation directives.
// Deliberately entry-only; see NewWithImports step 3.
func (e *Elaborator) collectEntryDirectives(program *ast.Program) {
	for _, stmt := range program.Statements {
		if d, ok := stmt.(*ast.DirectiveStatement); ok {
			e.output.Directives = append(e.output.Directives, d)
		}
	}
}
