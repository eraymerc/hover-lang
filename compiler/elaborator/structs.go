package elaborator

import (
	"fmt"
	"sort"

	"hover/compiler/ast"
)

// buildStructRegistry populates output.Structs from every loaded file's
// top-level struct declarations.
//
// Unlike buildModuleScopes/buildFunctionScopes, this is deliberately a
// single FLAT namespace, not scoped per file. A module/function reference
// carries file context at its call site (LogicObject.File / a module
// instantiation's declaring file), which is what lets those two be scoped
// per file — but a struct type name is just the bare Base string inside
// ast.Type, referenced with no file context alongside it anywhere in the
// pipeline. Giving structs the same per-file treatment would mean threading
// "which file does this Type.Base resolve against" through every type
// reference in the compiler, which the value of cross-file struct name
// reuse doesn't justify. The cost is the ordinary one for a flat namespace:
// two files declaring a struct with the same name collide, exactly like two
// files in the same directory declaring the same module name today. Struct
// types are consequently not alias-qualifiable (no `M.Point`) — a
// documented limitation, not an oversight.
func (e *Elaborator) buildStructRegistry(files map[string]*ImportedFile) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths) // deterministic error messages and Structs iteration

	for _, path := range paths {
		for _, stmt := range files[path].Program.Statements {
			sd, ok := stmt.(*ast.StructDeclStatement)
			if !ok {
				continue
			}
			if prev, dup := e.output.Structs[sd.Name]; dup && prev != sd {
				return fmt.Errorf("struct '%s' is declared twice — %s line %d and %s line %d",
					sd.Name, e.structFile[prev], prev.Line(), path, sd.Line())
			}
			e.output.Structs[sd.Name] = sd
			e.structFile[sd] = path
		}
	}
	return nil
}
