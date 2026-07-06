package ast

import (
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// STRUCTURED TYPE
//
// Type is the single structured representation of a Hover type, produced
// once by the parser and consumed as-is by semantic analysis, the
// elaborator, and codegen.
//
// It replaces the old string type-language ("unsigned int*[4]") that the
// parser flattened types into — which then had to be re-parsed by three
// independent shadow parsers: semantic.getBaseType/stripOneArrayDim,
// codegen.baseHoverType, and codegen.parseHoverType. All three are gone;
// String() below exists ONLY for display in error messages and debug
// output, never as an interchange format to be parsed back.
//
// Base also carries the handful of pseudo-types semantic analysis needs
// for expression inference ("number" for untyped literals, "unknown" for
// already-errored expressions, plus "wire"/"func"/"module" for symbols) —
// one representation for the whole pipeline, rather than a parallel
// semantic-only type system that would need converting at each boundary.
// ─────────────────────────────────────────────────────────────────────────────

type Type struct {
	Base  string // "double","float","int","unsigned int","wire","number","unknown","func","module"
	Stars int    // pointer levels: double** -> 2
	Dims  []int  // array dimensions, outer→inner: double[2][3] -> {2, 3}
}

// Pseudo-type and builtin-type constants. Scalar value types get real
// constructors below since they can carry stars/dims.
var (
	TDouble  = Type{Base: "double"}
	TNumber  = Type{Base: "number"}  // untyped numeric literal
	TUnknown = Type{Base: "unknown"} // an expression that already errored
	TWire    = Type{Base: "wire"}
	TFunc    = Type{Base: "func"}
	TModule  = Type{Base: "module"}
)

func (t Type) IsWire() bool    { return t.Base == "wire" }
func (t Type) IsArray() bool   { return len(t.Dims) > 0 }
func (t Type) IsPointer() bool { return t.Stars > 0 && len(t.Dims) == 0 }
func (t Type) IsScalar() bool  { return t.Stars == 0 && len(t.Dims) == 0 }

// Elem returns the bare element type — base only, no stars, no dims.
func (t Type) Elem() Type { return Type{Base: t.Base} }

// AddrOf returns the type of &expr: one more pointer level.
func (t Type) AddrOf() Type {
	t.Dims = cloneDims(t.Dims)
	t.Stars++
	return t
}

// Deref returns the type of *expr: one less pointer level. Dereferencing
// a non-pointer is the caller's error to report; the base type is
// returned so downstream inference can continue.
func (t Type) Deref() Type {
	if t.Stars > 0 {
		t.Dims = cloneDims(t.Dims)
		t.Stars--
		return t
	}
	return t.Elem()
}

// IndexOnce returns the type of expr[i]: the outermost array dimension is
// removed, mirroring what one subscript does in C ("double[2][3]" ->
// "double[3]" -> "double"). Indexing a pointer removes a pointer level.
func (t Type) IndexOnce() Type {
	if len(t.Dims) > 0 {
		t.Dims = cloneDims(t.Dims)[1:]
		return t
	}
	if t.Stars > 0 {
		t.Stars--
	}
	return t
}

// String renders the type for error messages and debug output —
// "unsigned int*[4]" — matching the old string type-language's spelling.
// DISPLAY ONLY: nothing may parse this back (that was the whole disease).
func (t Type) String() string {
	var b strings.Builder
	b.WriteString(t.Base)
	b.WriteString(strings.Repeat("*", t.Stars))
	for _, d := range t.Dims {
		b.WriteString("[")
		b.WriteString(strconv.Itoa(d))
		b.WriteString("]")
	}
	return b.String()
}

func cloneDims(d []int) []int {
	if d == nil {
		return nil
	}
	return append([]int(nil), d...)
}
