package codegen

import "strings"

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
	CInt  // maps to int64_t
	CUInt // maps to uint64_t
)

// String returns the C++ type keyword to emit for a declaration.
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
	}
	return "double"
}

// hoverTypeToCType maps a Hover declared-type string (as stored in
// ast.LocalDeclStatement.Type, ast.FuncParam.Type, ast.FuncDeclStatement.ReturnType)
// to its CType. Array suffixes ("double[4]") are stripped first — see
// baseHoverType — since the element type is what determines arithmetic
// behavior; array-ness itself doesn't affect promotion rules.
//
// "wire" has no CType — wires are topological, never stored as runtime
// values, and callers must not call this on a wire-typed declaration.
// "number" (the type of a bare literal, from semantic's untyped-literal
// convention) defaults to CDouble — matching the existing behavior where
// every literal was emitted as a double before this type system existed,
// and avoiding a forced re-typing of every numeric literal in existing
// Hover source.
func hoverTypeToCType(hoverType string) CType {
	base := baseHoverType(hoverType)
	switch base {
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

// baseHoverType strips an array suffix like "[4]" from a Hover type string,
// mirroring semantic.getBaseType's behavior on the codegen side.
func baseHoverType(t string) string {
	if idx := strings.IndexByte(t, '['); idx != -1 {
		t = t[:idx]
	}
	t = strings.TrimRight(t, "*")
	return strings.TrimSpace(t)
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
