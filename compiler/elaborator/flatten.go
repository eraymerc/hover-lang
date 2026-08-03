package elaborator

import (
	"fmt"
	"hover/compiler/ast"
	"hover/compiler/token"
)

func (e *Elaborator) flattenModule(mod *ast.ModuleDeclStatement, prefix string, params map[string]float64, ports map[string]string, domain token.Type) {
	if mod == nil {
		return
	}

	// Module references in this body resolve against the import list of the
	// file that DECLARED mod, not the entry file's — so a library can import
	// its own dependencies and choose its own aliases. See scope.go.
	scope := e.scopeFor(mod)

	// The declaring file also selects the FUNCTION namespace every statement
	// below is emitted against (LogicObject.File → ElaboratedProgram.FuncScopes),
	// for the same reason: a library's fabs() call must find the library's own
	// <math/math.hvr> import, not whatever the consumer happened to import.
	declFile := e.entryFile
	if p, ok := e.declFile[mod]; ok {
		declFile = p
	}

	for _, stmt := range mod.Body.Body {
		switch s := stmt.(type) {
		case *ast.ModuleInstStatement:
			target, ok := scope.resolveModule(s.ModuleName)
			if !ok {
				e.errors = append(e.errors, fmt.Sprintf("Line %d: undeclared module '%s'%s",
					s.Line(), s.ModuleName, describeScope(scope)))
				continue
			}

			// Module instantiation cycle check: if this declaration is already
			// being expanded somewhere up the current call stack, this
			// instantiation would recurse forever (A instantiates B
			// instantiates A, directly or through any chain of
			// intermediate modules). This is unrelated to the loader's
			// file-import cycle check — a module cycle can exist entirely
			// within one file, with no imports involved.
			//
			// Keyed by the resolved DECLARATION rather than the reference
			// spelling, so that two files reaching the same module under
			// different aliases still share one cycle key.
			if e.expanding[target] {
				e.errors = append(e.errors, fmt.Sprintf(
					"Line %d: module instantiation cycle detected — '%s' is already being expanded (it instantiates itself, directly or through other modules)",
					s.Line(), s.ModuleName))
				continue
			}

			if len(s.StaticArgs) != len(target.StaticArgs) {
				e.errors = append(e.errors, fmt.Sprintf("Line %d: parameter mismatch for module '%s' (expected %d, got %d)",
					s.Line(), s.ModuleName, len(target.StaticArgs), len(s.StaticArgs)))
				continue
			}

			childParams := make(map[string]float64)
			for i, arg := range s.StaticArgs {
				if i < len(target.StaticArgs) {
					childParams[target.StaticArgs[i].Name] = e.evalStatic(arg, params)
				}
			}

			expectedPorts := len(target.PhysPorts) + len(target.LogicArgs)
			actualPorts := len(s.PhysArgs) + len(s.LogicArgs)
			if actualPorts != expectedPorts {
				e.errors = append(e.errors, fmt.Sprintf("Line %d: port mismatch for module '%s' (expected %d, got %d)",
					s.Line(), s.ModuleName, expectedPorts, actualPorts))
				continue
			}

			childPorts := make(map[string]string)
			for i, arg := range s.LogicArgs {
				if i < len(target.LogicArgs) {
					childPorts[target.LogicArgs[i].Name] = e.mangleExpression(arg, prefix, ports)
				}
			}
			for i, arg := range s.PhysArgs {
				if i < len(target.PhysPorts) {
					childPorts[target.PhysPorts[i]] = e.mangleNode(arg.String(), prefix, ports)
				}
			}

			e.expanding[target] = true
			e.flattenModule(target, prefix+"."+s.InstanceName, childParams, childPorts, target.Token.Type)
			delete(e.expanding, target)

		case *ast.PhysicalPrimitiveStatement:
			physIdx := len(e.output.Physicals)

			// Split [] into wire terminals and, for CCCS/CCVS, the trailing
			// controlling-element reference. The sense entry must NOT end up in
			// Nodes: codegen's emitRegisterCall calls sys->register_node on
			// every entry there, which would mint a bogus MNA row named after
			// an element that is not a net at all.
			physArgs := s.PhysArgs
			var senseArg ast.Expression
			if s.IsCurrentControlled() && len(physArgs) > 0 {
				senseArg = physArgs[len(physArgs)-1]
				physArgs = physArgs[:len(physArgs)-1]
			}

			nodes := []string{}
			for _, n := range physArgs {
				nodes = append(nodes, e.mangleNode(n.String(), prefix, ports))
			}
			pMap := make(map[string]float64)
			for i, arg := range s.StaticArgs {
				pMap[fmt.Sprintf("param%d", i)] = e.evalStatic(arg, params)
			}

			// Capture the controlling logic signal for driven primitives.
			// voltage_source / current_source use LogicArgs[0] as their control.
			//
			// A literal control — current_source<>(0.01) — is NOT a signal, and
			// must not travel this path. mangleExpression happily returns the
			// digit string "0.01" as if it were a name (its NumberExpression
			// case), and codegen's phase_b then mangles that into an identifier
			// (mangle("0.01") == "v10201") and emits
			// api_set_current_source(..., v10201), which nothing declares. The
			// C++ compiler reported an undeclared identifier three layers away
			// from the source line that caused it.
			//
			// Folding the constant into param0 instead makes `<>(0.01)` mean
			// exactly what `<0.01>()` means — a fixed source stamped once at
			// build time with no per-step update, which is the only sensible
			// reading of a compile-time constant anyway.
			ctrlSignal := ""
			if len(s.LogicArgs) > 0 {
				if val, isConst := literalValue(s.LogicArgs[0]); isConst {
					pMap["param0"] = val
				} else {
					ctrlSignal = e.mangleExpression(s.LogicArgs[0], prefix, ports)
				}
			}

			name, userNamed := e.elementName(s, prefix, physIdx)

			e.output.Physicals = append(e.output.Physicals, PhysicalObject{
				Type:       s.PrimType,
				Name:       name,
				Parameters: pMap,
				Nodes:      nodes,
				CtrlSignal: ctrlSignal,
				UserNamed:  userNamed,
				Line:       s.Line(),
			})
			e.elementIndex[name] = physIdx

			if senseArg != nil {
				// A sense reference is an ELEMENT path, never a wire, so it
				// deliberately skips mangleNode: no port lookup, no gnd special
				// case. A bare "vsense" means the element of that name in this
				// instance; "probe.vsense" reaches into a child instance. Both
				// are just prefix + "." + path once flattened, which is exactly
				// the rule elementName applied when creating the name.
				path, ok := elementPath(senseArg)
				if !ok {
					e.errors = append(e.errors, fmt.Sprintf(
						"Line %d: %s's sense element must be an element name (e.g. vsense or probe.vsense), got '%s'",
						s.Line(), s.PrimType, senseArg.String()))
				} else {
					e.pendingSense = append(e.pendingSense, senseRef{
						physIdx: physIdx,
						target:  prefix + "." + path,
						raw:     path,
						line:    s.Line(),
					})
				}
			}

		case *ast.LocalDeclStatement:
			if s.Type.IsWire() {
				for _, d := range s.Decls {
					e.output.Symbols[prefix+"."+d.Name] = ast.TWire
				}
				continue
			}
			for _, d := range s.Decls {
				mangled := prefix + "." + d.Name
				e.output.Symbols[mangled] = s.Type
				e.output.Logic = append(e.output.Logic, LogicObject{
					Source: s,
					Writes: []string{mangled},
					Reads:  e.findReads(d.Value, prefix, ports),
					Prefix: prefix,
					Params: copyMapSF(params),
					Ports:  copyMapSS(ports),
					File:   declFile,
					Domain: domain,
				})
			}

		case *ast.AssignmentStatement:
			e.output.Logic = append(e.output.Logic, LogicObject{
				Source: s,
				Writes: []string{e.mangleExpression(s.Left, prefix, ports)},
				Reads:  e.findReads(s.Right, prefix, ports),
				Prefix: prefix,
				Params: copyMapSF(params),
				Ports:  copyMapSS(ports),
				File:   declFile,
				Domain: domain,
			})

		case *ast.ExpressionStatement:
			argRefs := e.findReads(s.Expression, prefix, ports)
			e.output.Logic = append(e.output.Logic, LogicObject{
				Source: s,
				Reads:  argRefs,
				Writes: argRefs,
				Prefix: prefix,
				Params: copyMapSF(params),
				Ports:  copyMapSS(ports),
				File:   declFile,
				Domain: domain,
			})

		case *ast.IfStatement, *ast.WhileStatement:
			e.output.Logic = append(e.output.Logic, LogicObject{
				Source: s,
				Writes: e.findWrites(s, prefix, ports),
				Reads:  e.findReads(s, prefix, ports),
				Prefix: prefix,
				Params: copyMapSF(params),
				Ports:  copyMapSS(ports),
				File:   declFile,
				Domain: domain,
			})
		}
	}
}
