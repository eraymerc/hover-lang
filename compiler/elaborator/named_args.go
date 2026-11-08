package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"strings"
)

// bindStaticArgs binds a module instantiation's <...> arguments to the target
// declaration's static parameters and returns the child's parameter map.
//
// Rules (Python's): positional arguments first, then name=value; a parameter
// may be bound once; any parameter left unbound takes the default from its
// declaration (`<double tau = 1m>`) and is an error if it has none. Defaults
// are evaluated in declaration order against the child's map, so a default may
// refer to an earlier parameter: `<double R, double P = R * 2>`.
func (e *Elaborator) bindStaticArgs(s *ast.ModuleInstStatement, target *ast.ModuleDeclStatement,
	params map[string]float64) (map[string]float64, bool) {

	child := make(map[string]float64)
	bound := make(map[string]bool)
	index := make(map[string]int, len(target.StaticArgs))
	for i, d := range target.StaticArgs {
		index[d.Name] = i
	}
	fail := func(format string, a ...interface{}) (map[string]float64, bool) {
		e.errors = append(e.errors, fmt.Sprintf("Line %d: module '%s': ", s.Line(), s.ModuleName)+
			fmt.Sprintf(format, a...))
		return nil, false
	}

	seenNamed := false
	positional := 0
	for _, arg := range s.StaticArgs {
		na, isNamed := arg.(*ast.NamedArgExpression)
		if !isNamed {
			if seenNamed {
				return fail("positional argument '%s' after a named argument — put positional arguments first",
					arg.String())
			}
			if positional >= len(target.StaticArgs) {
				return fail("too many parameters (expected at most %d, got %d)",
					len(target.StaticArgs), countPositional(s.StaticArgs))
			}
			name := target.StaticArgs[positional].Name
			child[name] = e.evalStatic(arg, params)
			bound[name] = true
			positional++
			continue
		}
		seenNamed = true
		if _, ok := index[na.Name]; !ok {
			return fail("no parameter named '%s' (parameters: %s)", na.Name, declNames(target))
		}
		if bound[na.Name] {
			return fail("parameter '%s' given more than once", na.Name)
		}
		child[na.Name] = e.evalStatic(na.Value, params)
		bound[na.Name] = true
	}

	for _, d := range target.StaticArgs {
		if bound[d.Name] {
			continue
		}
		if d.Value == nil {
			return fail("missing parameter '%s' (no default value; parameters: %s)", d.Name, declNames(target))
		}
		child[d.Name] = e.evalStatic(d.Value, child)
	}
	return child, true
}

func countPositional(args []ast.Expression) int {
	n := 0
	for _, a := range args {
		if _, ok := a.(*ast.NamedArgExpression); !ok {
			n++
		}
	}
	return n
}

func declNames(m *ast.ModuleDeclStatement) string {
	if len(m.StaticArgs) == 0 {
		return "none"
	}
	names := make([]string, len(m.StaticArgs))
	for i, d := range m.StaticArgs {
		names[i] = d.Name
	}
	return strings.Join(names, ", ")
}

// primitiveNamedParams lists the named <...> parameters each primitive
// accepts. Only the independent sources take any: ac (small-signal magnitude
// for .ac, default 0) and ac_phase (degrees, default 0). Both are static by
// construction — they live in <...>, which evalStatic folds to a number.
var primitiveNamedParams = map[string][]string{
	"voltage_source": {"ac", "ac_phase"},
	"current_source": {"ac", "ac_phase"},
}

// bindPrimitiveArgs folds a primitive's <...> list into pMap: positional
// arguments become param0, param1, ... exactly as before, named ones are
// stored under their own name. Returns false after reporting an error.
func (e *Elaborator) bindPrimitiveArgs(s *ast.PhysicalPrimitiveStatement, params map[string]float64,
	pMap map[string]float64) bool {

	allowed := primitiveNamedParams[strings.ToLower(s.PrimType)]
	seenNamed := false
	positional := 0
	for _, arg := range s.StaticArgs {
		na, isNamed := arg.(*ast.NamedArgExpression)
		if !isNamed {
			if seenNamed {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: %s: positional argument '%s' after a named argument — put positional arguments first",
					s.Line(), s.PrimType, arg.String()))
				return false
			}
			pMap[fmt.Sprintf("param%d", positional)] = e.evalStatic(arg, params)
			positional++
			continue
		}
		seenNamed = true
		ok := false
		for _, a := range allowed {
			if a == na.Name {
				ok = true
			}
		}
		if !ok {
			hint := "it takes no named parameters"
			if len(allowed) > 0 {
				hint = "named parameters: " + strings.Join(allowed, ", ")
			}
			e.errors = append(e.errors, fmt.Sprintf("Line %d: %s has no parameter named '%s' (%s)",
				s.Line(), s.PrimType, na.Name, hint))
			return false
		}
		if _, dup := pMap[na.Name]; dup {
			e.errors = append(e.errors, fmt.Sprintf("Line %d: %s: parameter '%s' given more than once",
				s.Line(), s.PrimType, na.Name))
			return false
		}
		pMap[na.Name] = e.evalStatic(na.Value, params)
	}
	return true
}
