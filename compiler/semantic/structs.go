package semantic

import ast "hover/compiler/ast"

// StructInfo is the semantic-layer record of one declared struct type: its
// name plus its ordered field list, as written in the source declaration.
type StructInfo struct {
	Name   string
	Fields []ast.StructField
}

// fieldByName looks up a field by name, returning its declared ast.Type.
func (s *StructInfo) fieldByName(name string) (ast.StructField, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return ast.StructField{}, false
}

// isBuiltinScalarBase reports whether a type's Base names one of Hover's
// primitive scalar types — the only field types a struct field may have
// besides another (previously declared) struct name. Kept local to this
// file since nothing else in semantic needs a builtin-base whitelist today.
func isBuiltinScalarBase(base string) bool {
	switch base {
	case "double", "float", "int", "unsigned int":
		return true
	}
	return false
}

// registerStructDecl validates and registers one struct declaration into
// a.structs. Field types are restricted to primitives, fixed-size arrays of
// primitives, or a struct name already registered by this point — the same
// discipline C uses for nested structs, which as a side effect makes
// self-referential/cyclic structs impossible to construct in the first
// place (a struct can only reference structs declared strictly before it),
// so no separate cycle-detection pass is needed.
func (a *Analyzer) registerStructDecl(node *ast.StructDeclStatement) {
	if _, exists := a.structs[node.Name]; exists {
		a.addError(node, "duplicate struct declaration '"+node.Name+"'")
		return
	}
	seen := map[string]bool{}
	for _, f := range node.Fields {
		if seen[f.Name] {
			a.addError(node, "struct '"+node.Name+"' declares field '"+f.Name+"' more than once")
			continue
		}
		seen[f.Name] = true

		if f.Type.IsWire() || f.Type.IsElement() {
			a.addError(node, "field '"+f.Name+"' of struct '"+node.Name+"' cannot be a wire or circuit element")
			continue
		}
		if isBuiltinScalarBase(f.Type.Base) {
			continue
		}
		if _, ok := a.structs[f.Type.Base]; ok {
			continue
		}
		a.addError(node, "field '"+f.Name+"' of struct '"+node.Name+"' has unknown type '"+f.Type.String()+
			"' — struct fields must be a primitive numeric type or a struct declared earlier in the file")
	}
	a.structs[node.Name] = &StructInfo{Name: node.Name, Fields: node.Fields}
}

// RegisterImportedStructs pre-registers every top-level StructDeclStatement
// found in an imported file's AST, before Analyze() walks the entry file —
// the struct-declaration counterpart of RegisterImportedFunctions, needed
// for exactly the same reason: a struct type used only by name (as
// ast.Type.Base) has no per-statement registration as the entry file is
// walked, so a struct declared in an imported file would otherwise be
// unknown here even though the elaborator resolves it correctly.
//
// Struct types are NOT alias-qualifiable in this version (no `M.Point`) —
// there's no grammar slot for it, since a type reference is a single bare
// Base string. Both bare (`import "x.hvr";`) and selective
// (`from "x.hvr" import Point;`) imports land the bare name in this
// registry, matching how RegisterImportedFunctions/RegisterSelectedFunctions
// already treat functions.
func (a *Analyzer) RegisterImportedStructs(importedProgram *ast.Program) {
	for _, stmt := range importedProgram.Statements {
		if sd, ok := stmt.(*ast.StructDeclStatement); ok {
			a.registerStructDecl(sd)
		}
	}
}
