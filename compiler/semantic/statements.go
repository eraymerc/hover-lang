package semantic

import (
	"fmt"
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

func (a *Analyzer) checkStatement(stmt ast.Statement) {
	switch node := stmt.(type) {

	case *ast.StructDeclStatement:
		a.registerStructDecl(node)

	case *ast.LocalDeclStatement:
		if a.currentDomain == token.MODULE {
			for _, decl := range node.Decls {
				if !isStructuralInit(decl.Value) {
					a.addError(node, fmt.Sprintf(
						"'%s' — structural modules cannot compute values. Move math into an analog or digital module.",
						decl.Name,
					))
				}
			}
		}

		if a.currentDomain == token.MODULE && !node.Type.IsWire() && !node.IsState {
			for _, decl := range node.Decls {
				a.addError(node, fmt.Sprintf(
					"'%s' — variables in structural modules must be declared as 'state'. "+
						"Plain variables reset every step; use 'state double %s = 0' instead.",
					decl.Name, decl.Name,
				))
			}
		}

		if a.currentDomain == token.ANALOG && node.IsState {
			a.addError(node, "Analog modules cannot have 'state' variables — analog models are stateless.")
		}

		for _, decl := range node.Decls {
			if decl.Value != nil {
				a.checkExpression(decl.Value)
			}
			// A wire or variable sharing a name with a circuit element would
			// make V(x)/I(x)/.save(x) ambiguous. Caught here for the precise
			// line number; the elaborator repeats the check across the whole
			// flattened design, since semantic analysis only walks the entry
			// file and never sees imported modules.
			if prev, ok := a.currentScope.Resolve(decl.Name); ok && prev.Type.IsElement() {
				a.addError(node, fmt.Sprintf(
					"'%s' collides with a circuit element of the same name declared in this module",
					decl.Name))
			}
			a.currentScope.Define(&Symbol{Name: decl.Name, Type: node.Type, IsState: node.IsState})
		}
	case *ast.AssignmentStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain math or logic. They are only for wiring components together.")
		}
		a.checkExpression(node.Left)
		a.checkExpression(node.Right)
	case *ast.BlockStatement:
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, s := range node.Body {
			a.checkStatement(s)
		}
		a.currentScope = parent
	case *ast.IfStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain 'if' statements.")
		}

		condType := a.checkExpression(node.Condition)
		if condType.IsWire() {
			a.addError(node, "Condition expression cannot be a physical wire. Use V() or I() to read its value.")
		}

		prev := a.inLogicBlock
		a.inLogicBlock = true
		a.checkStatement(node.Consequence)
		for _, alt := range node.Alternatives {
			a.checkExpression(alt.Condition)
			a.checkStatement(alt.Body)
		}
		if node.Alternative != nil {
			a.checkStatement(node.Alternative)
		}
		a.inLogicBlock = prev
	case *ast.WhileStatement:
		if a.currentDomain == token.MODULE {
			a.addError(node, "Structural modules cannot contain 'while' loops.")
		}
		if a.currentDomain == token.ANALOG {
			a.addError(node, "Analog modules cannot contain 'while' loops — risk of infinite loop inside Newton-Raphson convergence.")
		}
		a.checkExpression(node.Condition)

		condType := a.checkExpression(node.Condition)
		if condType.IsWire() {
			a.addError(node, "Condition expression cannot be a physical wire.")
		}

		a.checkStatement(node.Body)
	case *ast.ReturnStatement:
		retType := a.checkExpression(node.ReturnValue)
		if retType.IsWire() {
			a.addError(node, "Functions cannot return physical wires.")
		}
	case *ast.ModuleDeclStatement:
		a.currentScope.Define(&Symbol{Name: node.Name, Type: ast.TModule})
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, arg := range node.StaticArgs {
			if _, isStruct := a.structs[arg.Type.Base]; isStruct {
				a.addError(node, fmt.Sprintf(
					"static parameter '%s' cannot be struct-typed ('%s') — module static parameters must stay physical/numeric",
					arg.Name, arg.Type))
			}
			a.currentScope.Define(&Symbol{Name: arg.Name, Type: arg.Type})
		}
		for _, arg := range node.LogicArgs {
			if _, isStruct := a.structs[arg.Type.Base]; isStruct {
				a.addError(node, fmt.Sprintf(
					"port '%s' cannot be struct-typed ('%s') — module ports must be wires",
					arg.Name, arg.Type))
			}
			a.currentScope.Define(&Symbol{Name: arg.Name, Type: arg.Type})
		}
		for _, port := range node.PhysPorts {
			a.currentScope.Define(&Symbol{Name: port, Type: ast.TWire})
		}

		// Named circuit elements go into the module's scope UP FRONT, not as
		// the body walk reaches them. A logic block may legitimately read
		// I(vsense) above the line declaring vsense, and a CCCS may sense an
		// element declared later — netlist construction is order-independent
		// (register_netlist runs entirely before stamp_netlist), so source
		// order carries no meaning here and forward references must not error.
		for _, s := range bodyStatements(node.Body) {
			pp, ok := s.(*ast.PhysicalPrimitiveStatement)
			if !ok || pp.Name == "" {
				continue
			}
			if prev, exists := a.currentScope.Store[pp.Name]; exists {
				a.addError(pp, fmt.Sprintf(
					"duplicate name '%s' in module '%s' — already used by a %s",
					pp.Name, node.Name, describeSymbol(prev)))
				continue
			}
			a.currentScope.Define(&Symbol{Name: pp.Name, Type: ast.TElement})
		}

		prevDomain := a.currentDomain
		a.currentDomain = node.Token.Type

		a.checkStatement(node.Body)

		a.currentDomain = prevDomain
		a.currentScope = parent
	case *ast.FuncDeclStatement:
		a.currentScope.Define(&Symbol{Name: node.Name, Type: ast.TFunc})
		if node.IsExtern {
			// extern func crosses the C++ FFI boundary, where Hover structs
			// don't exist as a real C++ type it can name — the sanctioned
			// way to move aggregate data across that boundary is the
			// wrapper-function/opaque-pointer pattern documented in the
			// user manual ("Handling Structs and Complex Types"), not a
			// native struct parameter/return.
			if _, isStruct := a.structs[node.ReturnType.Base]; isStruct {
				a.addError(node, fmt.Sprintf(
					"extern func '%s' cannot return a struct ('%s') — use the opaque-pointer pattern to cross the FFI boundary",
					node.Name, node.ReturnType))
			}
			for _, p := range node.Parameters {
				if _, isStruct := a.structs[p.Type.Base]; isStruct {
					a.addError(node, fmt.Sprintf(
						"extern func '%s' parameter '%s' cannot be struct-typed ('%s') — use the opaque-pointer pattern to cross the FFI boundary",
						node.Name, p.Name, p.Type))
				}
			}
		}
		if node.Body == nil { // extern: nothing to analyze
			return
		}
		parent := a.currentScope
		a.currentScope = NewScope(parent)
		for _, p := range node.Parameters {
			a.currentScope.Define(&Symbol{Name: p.Name, Type: p.Type})
		}
		a.checkStatement(node.Body)
		a.currentScope = parent
	case *ast.ModuleInstStatement, *ast.PhysicalPrimitiveStatement:
		if _, isPhys := node.(*ast.PhysicalPrimitiveStatement); isPhys && a.currentDomain == token.DIGITAL {
			a.addError(node, "Digital modules cannot instantiate analog hardware (like R, C, or voltage_source).")
		}

		var physArgs []ast.Expression
		if mi, ok := stmt.(*ast.ModuleInstStatement); ok {
			physArgs = mi.PhysArgs
		}
		prim, isPrim := stmt.(*ast.PhysicalPrimitiveStatement)
		if isPrim {
			physArgs = prim.PhysArgs
		}

		// nWires is how many leading [] entries are wire terminals. For a
		// current-controlled source the LAST entry is an element reference
		// instead — SPICE-style `CCCS<beta>() [out_p, out_n, vsense]` — and is
		// deliberately left unchecked here. It may name an element declared
		// later in the module, or reach into a child instance via a dotted
		// path whose first segment is an instance name semantic analysis never
		// defines as a symbol. The elaborator validates it for real, against
		// the flattened design (elements.go: resolveSenseElements).
		nWires := len(physArgs)
		if isPrim {
			if want, ok := physArity[prim.PrimType]; ok && len(physArgs) != want {
				hint := ""
				if prim.IsCurrentControlled() {
					hint = " — [out_p, out_n, sense_element]"
				}
				a.addError(stmt, fmt.Sprintf("%s expects %d terminals in [], got %d%s",
					prim.PrimType, want, len(physArgs), hint))
			}
			if prim.IsCurrentControlled() && nWires > 0 {
				nWires--
			}
		}

		for _, arg := range physArgs[:nWires] {
			t := a.checkExpression(arg)
			if !t.IsWire() && t.Base != "unknown" {
				a.addError(stmt, fmt.Sprintf("Physical port must be a wire, got '%s'", t))
			}
		}
	}
}
