package elaborator

import (
	"hover/compiler/ast"
	"hover/compiler/loader"
	"path/filepath"
	"strconv"
	"strings"
)

// engineeringSuffixes must stay in lockstep with the suffix set the lexer
// agrees to consume (lexer.go, readNumber) — a suffix the lexer accepts but
// this table doesn't would silently parse as its bare number, scaling a value
// by 1e15.
//
// Ordered longest-first and iterated in order rather than as a map: "Meg" has
// to be tested before any single character it ends with, and map iteration
// order in Go is deliberately randomized, so a table that happens not to
// collide today would be a latent nondeterministic bug the moment one is
// added.
var engineeringSuffixes = []struct {
	suffix string
	scale  float64
}{
	{"Meg", 1e6},
	{"f", 1e-15},
	{"p", 1e-12},
	{"n", 1e-9},
	{"u", 1e-6},
	{"m", 1e-3},
	{"k", 1e3},
	{"G", 1e9},
}

func ParseEngineering(s string) float64 {
	for _, e := range engineeringSuffixes {
		if strings.HasSuffix(s, e.suffix) {
			val, _ := strconv.ParseFloat(strings.TrimSuffix(s, e.suffix), 64)
			return val * e.scale
		}
	}
	val, _ := strconv.ParseFloat(s, 64)
	return val
}

// literalValue reports whether exp is a compile-time numeric literal, and if
// so its value. Deliberately narrow: a bare number or a signed number, nothing
// else.
//
// The narrowness is the point. Callers use this to decide whether an
// expression is a CONSTANT or a SIGNAL NAME, and evalStatic cannot make that
// distinction — it resolves an unknown identifier to 0.0, so handing it
// `voltage_source<>(vsig)` would silently fold a live signal into a dead 0 V
// source. Recognizing only literal forms means anything else falls through to
// name resolution, where an unknown name is a reported error rather than a
// wrong answer.
func literalValue(exp ast.Expression) (float64, bool) {
	switch n := exp.(type) {
	case *ast.NumberExpression:
		return ParseEngineering(n.Value), true
	case *ast.UnaryExpression:
		v, ok := literalValue(n.Right)
		if !ok {
			return 0, false
		}
		switch n.Operator {
		case "-":
			return -v, true
		case "+":
			return v, true
		}
	}
	return 0, false
}

func copyMapSF(m map[string]float64) map[string]float64 {
	c := make(map[string]float64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func copyMapSS(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// splitQualifiedName splits a possibly-aliased name like "Motor.PMSM" into
// ("Motor", "PMSM", true). For an unqualified name like "PMSM", it returns
// ("", "PMSM", false).
//
// Only the FIRST dot matters — "Motor.PMSM" splits into "Motor" and "PMSM",
// not further. This matches how module/function names are written: aliases
// are always exactly one segment, since imports are non-transitive (you
// can't write "Motor.SubAlias.Thing").
func splitQualifiedName(name string) (alias string, bare string, isQualified bool) {
	idx := strings.IndexByte(name, '.')
	if idx == -1 {
		return "", name, false
	}
	return name[:idx], name[idx+1:], true
}

// resolveImportPathFor resolves an import path to the absolute file the
// loader would have loaded for it, by calling straight into the loader.
//
// This used to be a small local copy of the loader's logic, kept so the
// elaborator wouldn't depend on filesystem layout. That stopped being
// tenable once `import <pkg/...>` existed: package resolution consults a
// per-project table built from the lockfile, and two independent copies of
// the rule would silently resolve the same import to two different files —
// the elaborator would then report "import could not be resolved" for a file
// the loader read successfully. There is exactly one rule now.
func resolveImportPathFor(currentFilePath, importPath string, isSystem bool) string {
	return loader.ResolveImportPath(filepath.Dir(currentFilePath), importPath, isSystem)
}
