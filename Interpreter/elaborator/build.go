package elaborator

import (
	"fmt"
	"hover/Interpreter/token"
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
	main, ok := e.modules["main"]
	if !ok {
		return nil, fmt.Errorf("module 'main' not found")
	}

	for _, mod := range e.modules {
		if mod.Token.Type == token.ANALOG {
			e.processAnalogIdt(mod)
			e.processAnalogDdt(mod)
		}
	}

	e.flattenModule(main, "main", make(map[string]float64), make(map[string]string), main.Token.Type)

	if len(e.errors) > 0 {
		return nil, fmt.Errorf("elaboration failed:\n%s", strings.Join(e.errors, "\n"))
	}

	return e.output, nil
}
