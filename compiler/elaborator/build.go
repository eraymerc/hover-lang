package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"hover/compiler/token"
	"strings"
)

// Elaborate flattens the module hierarchy rooted at 'main' into the flat
// ElaboratedProgram that codegen consumes.
//
// Historical note: this used to also construct and stamp a Go-side
// *mna.System and return it as a second value — a leftover from the
// original Go interpreter backend. Since the compiler pipeline took over,
// codegen builds its own netlist (Codegen/netlist.go) and main.go
// discarded that second return entirely, so the Go MNA package and the
// stamping pass here were deleted.
func (e *Elaborator) Elaborate() (*ElaboratedProgram, error) {
	main, ok := e.entryScope.resolveModule("main")
	if !ok {
		return nil, fmt.Errorf("module 'main' not found")
	}

	// Macro rewriting applies to every analog module in the whole loaded set,
	// not just the entry file's — a library's analog module needs its idt()/
	// ddt() expanded whether or not the entry file can name it.
	for _, mod := range e.allModules() {
		if mod.Token.Type == token.ANALOG {
			e.processAnalogIdt(mod)
			e.processAnalogDdt(mod)
		}
	}

	// 1. Bootstrap main's StaticArgs (<> params) so hovercraft_emit can generate HVR_set_param_*
	mainParams := make(map[string]float64)
	for _, param := range main.StaticArgs {
		if param.Value != nil {
			mainParams[param.Name] = e.evalStatic(param.Value, nil)
		} else {
			mainParams[param.Name] = 0.0
		}
	}

	// 2. Bootstrap main's LogicArgs (() inputs) so hovercraft_emit can generate HVR_set_input_*
	mainPorts := make(map[string]string)
	var injectedDecls []ast.Statement
	for _, arg := range main.LogicArgs {
		mainPorts[arg.Name] = "main." + arg.Name
		// Inject a local declaration for the input so codegen allocates a backing C++ global
		// and tracks its type correctly.
		injectedDecls = append(injectedDecls, &ast.LocalDeclStatement{
			Token: main.Token,
			Type:  arg.Type,
			Decls: []*ast.VarDecl{
				{Name: arg.Name, Value: nil},
			},
		})
	}
	// Prepend the injected declarations to main's body
	if len(injectedDecls) > 0 {
		main.Body.Body = append(injectedDecls, main.Body.Body...)
	}

	e.flattenModule(main, "main", mainParams, mainPorts, main.Token.Type)

	// 3. Post-flatten validation (elements.go). Both passes need the COMPLETE
	//    design: a CCCS/CCVS sense reference legitimately points forward, and a
	//    wire colliding with an element name can be declared anywhere in the
	//    hierarchy — including in an imported file, which semantic analysis
	//    never walks.
	e.resolveSenseElements()
	e.checkElementNameCollisions()

	if len(e.errors) > 0 {
		return nil, fmt.Errorf("elaboration failed:\n%s", strings.Join(e.errors, "\n"))
	}

	return e.output, nil
}
