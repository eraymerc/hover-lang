package codegen

import (
	"fmt"
	"strconv"
	"strings"

	ast "hover/compiler/ast"
)

// ─────────────────────────────────────────────────────────────────────────────
// C++ DECLARATORS — ARRAY & POINTER AWARE
//
// ctype.go intentionally collapses any array/pointer Hover type down to its
// *element* CType, because arithmetic promotion only cares about the element.
// That is correct for expressions, but every place that emits a real C++
// DECLARATION (a file-scope global, a function-local, a parameter) needs the
// full shape of the type — otherwise `double[2]` is declared as a bare
// `double`, and indexing it (`arr[0]`) fails to compile with
// "subscripted value is not an array, pointer, or vector".
//
// hoverType below is that full shape: element CType + pointer levels + array
// dimensions. The cXxxDecl methods turn it into the C "declarator" form,
// following real C rules — pointer stars sit before the name, array
// dimensions after it, and an array parameter's outermost dimension decays
// to a pointer (so writes through it reach the caller, exactly like C).
// ─────────────────────────────────────────────────────────────────────────────

// hoverType is the fully-parsed form of a Hover declared-type string.
type hoverType struct {
	elem       CType  // element type (double / float / int64_t / uint64_t / struct)
	structName string // set iff elem == CStruct: the emitted C++ struct's name
	stars      int    // pointer levels: double** -> 2
	dims       []int  // array dimensions, outer→inner: double[2][3] -> {2, 3}
}

// hoverTypeOf converts the parser's structured ast.Type into codegen's
// hoverType (element CType + declarator shape). This is a field mapping,
// not a parse — the string grammar this function used to re-parse
// ("unsigned int*[4]") no longer exists anywhere in the pipeline.
//
// A method (not a free function) because hoverTypeToCType needs g.prog's
// struct registry to recognize a struct-typed Base.
func (g *generator) hoverTypeOf(t ast.Type) hoverType {
	dims := make([]int, len(t.Dims))
	copy(dims, t.Dims)
	elem := g.hoverTypeToCType(t)
	structName := ""
	if elem == CStruct {
		structName = t.Base
	}
	return hoverType{elem: elem, structName: structName, stars: t.Stars, dims: dims}
}

// typeKeyword returns the C++ type keyword this hoverType's element emits
// as — CType.String() for a scalar, or the real struct name for CStruct
// (CType.String() alone can't answer this: a bare CType carries no name).
func (h hoverType) typeKeyword() string {
	if h.elem == CStruct {
		return h.structName
	}
	return h.elem.String()
}

// isArray reports whether this type has at least one array dimension.
func (h hoverType) isArray() bool { return len(h.dims) > 0 }

// isPointer reports whether this type is a bare pointer (stars, no dims).
func (h hoverType) isPointer() bool { return h.stars > 0 && len(h.dims) == 0 }

// isScalar reports whether this is a plain non-array, non-pointer value.
func (h hoverType) isScalar() bool { return h.stars == 0 && len(h.dims) == 0 }

// elemCount returns the total number of scalar elements an array holds
// (product of its dimensions), or 1 for a scalar/pointer. Used to expand a
// scalar-fill initializer (double[2] arr = 1e-6) into the right element count.
func (h hoverType) elemCount() int {
	if len(h.dims) == 0 {
		return 1
	}
	n := 1
	for _, d := range h.dims {
		n *= d
	}
	return n
}

// cVarDecl produces the C++ declaration of a plain variable (a file-scope
// static or a function-local) with the given identifier — the C declarator:
// pointer stars before the name, array dimensions after it.
//
//	double            x       -> "double x"
//	double*           p       -> "double *p"
//	double[2]         arr     -> "double arr[2]"
//	double[2][3]      m       -> "double m[2][3]"
//	double*[4]        pa      -> "double *pa[4]"   (array of pointers, C reading)
func (h hoverType) cVarDecl(name string) string {
	decl := h.typeKeyword() + " " + strings.Repeat("*", h.stars) + name
	for _, d := range h.dims {
		decl += "[" + strconv.Itoa(d) + "]"
	}
	return decl
}

// cParamDecl produces the C++ parameter declaration. Arrays keep their
// array syntax (valid C; the outermost extent decays to a pointer, so the
// function can write through it and the caller sees the change — exactly the
// pass-by-reference-of-array behaviour Hover source relies on). Scalars and
// pointers are passed unchanged.
func (h hoverType) cParamDecl(name string) string {
	return h.cVarDecl(name)
}

// cReturnType produces the C++ return-type keyword. Arrays cannot be returned
// in C/C++, so an array return degrades to the element type (matching C);
// pointer returns are emitted faithfully.
func (h hoverType) cReturnType() string {
	return h.typeKeyword() + strings.Repeat("*", h.stars)
}

// cDecayedLocalDecl produces the declaration of the renamed function-local
// alias emitOneFunction creates for each parameter. Scalars are a plain copy
// (`double f_x = x;`). Arrays/pointers must alias the caller's storage rather
// than copy (C cannot copy an array by assignment, and writes must reach the
// caller), so the outermost array dimension decays to a pointer:
//
//	double           f_x   = x;       (scalar — copy)
//	double*          f_p   = p;       (pointer — alias)
//	double[2]        f_arr = arr;     -> "double *f_arr = arr;"
//	double[2][3]     f_m   = m;       -> "double (*f_m)[3] = m;"
func (h hoverType) cDecayedLocalDecl(name, init string) string {
	if !h.isArray() {
		// scalar or bare pointer: keep prior copy/alias behaviour
		return fmt.Sprintf("%s = %s;", h.cVarDecl(name), init)
	}
	inner := h.dims[1:]
	stars := h.stars + 1 // outermost dimension decays to one more pointer level
	if len(inner) == 0 {
		// 1-D array decays to a plain pointer: double *f_arr = arr;
		return fmt.Sprintf("%s %s%s = %s;",
			h.typeKeyword(), strings.Repeat("*", stars), name, init)
	}
	// >=2-D decays to pointer-to-array; the name needs parentheses:
	// double (*f_m)[3] = m;
	suffix := ""
	for _, d := range inner {
		suffix += "[" + strconv.Itoa(d) + "]"
	}
	return fmt.Sprintf("%s (%s%s)%s = %s;",
		h.typeKeyword(), strings.Repeat("*", stars), name, suffix, init)
}

// zeroValue renders a value-initialized ("zero") literal of this type, for
// contexts like a function's unreachable fallthrough return. Struct and
// array types use C++'s own empty-brace value-initialization (`TypeName{}`
// / `{}`), which zeroes every member/element without this needing to know
// their layout; pointers are a null literal; scalars go through
// formatTypedLiteral for the correct per-CType zero spelling (0 vs 0.0).
func (h hoverType) zeroValue() string {
	if h.elem == CStruct && !h.isArray() && h.stars == 0 {
		return h.structName + "{}"
	}
	if h.isArray() {
		return "{}"
	}
	if h.stars > 0 {
		return "0"
	}
	return formatTypedLiteral(0.0, h.elem)
}

// cFFIType renders the type as a C header would (C-natural element name plus
// pointer stars), for casting arguments passed to extern C functions. Arrays
// are treated as a single pointer (extern array params decay like C).
//
// Never valid for a struct-typed hoverType — semantic analysis rejects any
// struct type on an extern func's signature before codegen runs (structs
// don't cross the FFI boundary directly; see the opaque-pointer pattern in
// the user manual), so reaching this with elem == CStruct means that
// invariant was somehow bypassed. Fail loudly rather than emit a bogus C
// type name.
func (h hoverType) cFFIType() string {
	if h.elem == CStruct {
		panic("cFFIType called on a struct-typed hoverType (" + h.structName + ") — semantic analysis should have rejected this extern func signature")
	}
	stars := h.stars
	if h.isArray() {
		stars++
	}
	return h.elem.cName() + strings.Repeat("*", stars)
}
