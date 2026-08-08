package codegen

import ast "hover/compiler/ast"

// ─────────────────────────────────────────────────────────────────────────────
// C++ TYPE SYSTEM
//
// Hover's declared types (double, float, int, unsigned int) now map to
// genuinely distinct C++ types in generated code, rather than everything
// being a uniform `double`. This file defines that mapping plus the
// promotion rules used when two differently-typed operands meet in a
// binary expression — these rules are deliberately a faithful copy of
// C/C++'s real "usual arithmetic conversions", not a simplified subset,
// per the decision to preserve real C/C++ behavior throughout.
// ─────────────────────────────────────────────────────────────────────────────

// CType is the small, closed set of C++ types Hover variables can have.
// Array variants (e.g. "double[4]") share the same element CType — array-ness
// is tracked separately by the existing string-based Hover type, not here.
type CType int

const (
	CDouble CType = iota
	CFloat
	CInt   // maps to int64_t
	CUInt  // maps to uint64_t
	CStruct // a Hover-declared struct type; the real name lives in hoverType.structName, not here
)

// String returns the C++ type keyword to emit for a declaration. NOT valid
// for CStruct — a bare CType carries no struct name, so callers that know
// they may be holding a struct type must go through hoverType.typeKeyword()
// instead, which does have the name. This fallback exists only so a
// mis-derived cast involving CStruct fails loudly in the emitted C++ (a
// compile error there) rather than silently emitting plausible-looking but
// wrong code — see castToHover/emitCast, which are never expected to reach
// this for a well-typed program (semantic analysis rejects any expression
// that would mix a struct with something else before codegen runs).
func (c CType) String() string {
	switch c {
	case CDouble:
		return "double"
	case CFloat:
		return "float"
	case CInt:
		return "int64_t"
	case CUInt:
		return "uint64_t"
	case CStruct:
		return "/* INVALID: struct type used where a scalar cast was expected */"
	}
	return "double"
}

// hoverTypeToCType maps a Hover declared type to its CType. Only the BASE
// participates — the element type is what determines arithmetic behavior;
// array/pointer shape doesn't affect promotion rules (declarator.go tracks
// the full shape for declaration emission).
//
// "wire" has no CType — wires are topological, never stored as runtime
// values, and callers must not call this on a wire-typed declaration.
// "number" (the type of a bare literal, from semantic's untyped-literal
// convention) defaults to CDouble — matching the existing behavior where
// every literal was emitted as a double before this type system existed.
//
// A method (not a free function) so it can consult g.prog.Structs: an
// unrecognized Base used to silently default to CDouble, which became a
// real correctness hazard once struct names are valid Base values — this
// checks the struct registry FIRST so a struct type maps to CStruct
// instead of falling through to that default.
func (g *generator) hoverTypeToCType(t ast.Type) CType {
	if _, ok := g.prog.Structs[t.Base]; ok {
		return CStruct
	}
	switch t.Base {
	case "double", "number", "unknown", "":
		return CDouble
	case "float":
		return CFloat
	case "int":
		return CInt
	case "unsigned int", "unsigned":
		return CUInt
	}
	return CDouble
}

// promote computes the result CType of applying a binary operator to two
// operands of types a and b, following C/C++'s real usual arithmetic
// conversions:
//
//   - If either operand is double, result is double.
//   - Else if either operand is float, result is float.
//   - Else (both are some integer type):
//   - If either is unsigned, result is unsigned (the classic C rule:
//     mixing signed and unsigned promotes to unsigned, which is a
//     well-known footgun in real C/C++ but preserved here faithfully
//     per the decision to match real C/C++ behavior exactly).
//   - Otherwise both are signed int, result is int.
//
// op is accepted but currently unused by the floating/integer promotion
// itself — every arithmetic operator (+, -, *, /, %, &, |, ^) follows the
// same operand-promotion rule in C. op is threaded through so call sites
// can pass it for clarity and so future operator-specific exceptions have
// an obvious place to slot in without changing every call site's signature.
func promote(a, b CType, op string) CType {
	_ = op
	if a == CDouble || b == CDouble {
		return CDouble
	}
	if a == CFloat || b == CFloat {
		return CFloat
	}
	if a == CUInt || b == CUInt {
		return CUInt
	}
	return CInt
}

// isIntegerType reports whether c is one of Hover's two integer CTypes
// (CInt or CUInt) — used by codegen's bitwise-operator emission, which is
// only ever reached for genuinely integer-typed operands since the
// semantic analyzer already rejects double/float operands to &, |, ^, <<,
// >>, ~ before codegen runs. This helper exists mainly so codegen can
// assert that invariant rather than silently emitting wrong C++ if the
// semantic check is ever bypassed (e.g. a future direct-AST-to-codegen path
// that skips semantic analysis).
func isIntegerType(c CType) bool {
	return c == CInt || c == CUInt
}

// needsCast reports whether a value of type from being placed into a
// context expecting type to requires an explicit C++ cast to avoid
// relying on implicit conversion. Hover's explicit-casting philosophy
// inserts a cast at every type boundary, even ones C itself would convert
// implicitly without complaint (e.g. int → double) — this keeps generated
// code's intent visible and avoids any ambiguity about narrowing.
//
// Two struct-typed operands (from == to == CStruct) correctly report "no
// cast needed" here — C++ assigns/copies an aggregate memberwise via its
// implicit operator=, exactly the by-value semantics Hover structs need,
// with no cast involved. Semantic analysis guarantees a struct is only ever
// paired with an equal struct type by the time codegen sees it, so this
// bare CType comparison (which can't see the two structs' actual names) is
// safe in practice; a genuine mismatch would be a semantic-analysis bug,
// and would surface as a real C++ compile error rather than silently
// wrong code, per emitCast's defensive CType.String() fallback for CStruct.
func needsCast(from, to CType) bool {
	return from != to
}

// emitCast wraps expr in an explicit C++ cast to targetType, unless no
// cast is needed.
func emitCast(expr string, fromType, targetType CType) string {
	if !needsCast(fromType, targetType) {
		return expr
	}
	return "(" + targetType.String() + ")(" + expr + ")"
}

// cName returns the C-natural type keyword (used only at the FFI boundary,
// where headers use real `int`, not the int64_t Hover stores internally).
func (c CType) cName() string {
	switch c {
	case CDouble:
		return "double"
	case CFloat:
		return "float"
	case CInt:
		return "int"
	case CUInt:
		return "unsigned int"
	}
	return "double"
}

// castToHover casts a C expression of element type `from` into the Hover target
// type. Pointer targets get a C-style (reinterpret) cast — this bridges FFI
// pointer-type discrepancies (e.g. an extern `int*` return stored in Hover's
// `int64_t*`) and is a no-op for matching types. Scalars use the normal cast.
func castToHover(expr string, from CType, target hoverType) string {
	if target.isPointer() {
		return "(" + target.cReturnType() + ")(" + expr + ")"
	}
	return emitCast(expr, from, target.elem)
}
