package elaborator

import "hover/Interpreter/ast"

func (e *Elaborator) findReads(node ast.Node, prefix string, ports map[string]string) []string {
	reads := []string{}
	if node == nil {
		return reads
	}
	switch n := node.(type) {

	// ── Expressions ───────────────────────────────────────────────────────
	case *ast.IdentifierExpression:
		reads = append(reads, e.mangleNode(n.Value, prefix, ports))

	case *ast.UnaryExpression:
		reads = append(reads, e.findReads(n.Right, prefix, ports)...)

	case *ast.BinaryExpression:
		reads = append(reads, e.findReads(n.Left, prefix, ports)...)
		reads = append(reads, e.findReads(n.Right, prefix, ports)...)

	case *ast.CallExpression:
		for _, arg := range n.Arguments {
			reads = append(reads, e.findReads(arg, prefix, ports)...)
		}

	case *ast.IndexExpression:
		reads = append(reads, e.findReads(n.Left, prefix, ports)...)
		reads = append(reads, e.findReads(n.Index, prefix, ports)...)

	// ── Statements ────────────────────────────────────────────────────────
	case *ast.AssignmentStatement:
		reads = append(reads, e.findReads(n.Right, prefix, ports)...)

	case *ast.LocalDeclStatement:
		for _, d := range n.Decls {
			reads = append(reads, e.findReads(d.Value, prefix, ports)...)
		}

	case *ast.BlockStatement:
		for _, s := range n.Body {
			reads = append(reads, e.findReads(s, prefix, ports)...)
		}

	case *ast.IfStatement:
		reads = append(reads, e.findReads(n.Condition, prefix, ports)...)
		reads = append(reads, e.findReads(n.Consequence, prefix, ports)...)
		for _, alt := range n.Alternatives {
			reads = append(reads, e.findReads(alt.Condition, prefix, ports)...)
			reads = append(reads, e.findReads(alt.Body, prefix, ports)...)
		}
		if n.Alternative != nil {
			reads = append(reads, e.findReads(n.Alternative, prefix, ports)...)
		}

	case *ast.WhileStatement:
		reads = append(reads, e.findReads(n.Condition, prefix, ports)...)
		reads = append(reads, e.findReads(n.Body, prefix, ports)...)

	case *ast.ReturnStatement:
		reads = append(reads, e.findReads(n.ReturnValue, prefix, ports)...)
	}
	return reads
}

func (e *Elaborator) findWrites(node ast.Node, prefix string, ports map[string]string) []string {
	writes := []string{}
	if node == nil {
		return writes
	}
	switch n := node.(type) {

	case *ast.AssignmentStatement:
		writes = append(writes, e.mangleExpression(n.Left, prefix, ports))

	case *ast.LocalDeclStatement:
		for _, d := range n.Decls {
			writes = append(writes, prefix+"."+d.Name)
		}

	case *ast.BlockStatement:
		for _, s := range n.Body {
			writes = append(writes, e.findWrites(s, prefix, ports)...)
		}

	case *ast.IfStatement:
		writes = append(writes, e.findWrites(n.Consequence, prefix, ports)...)
		for _, alt := range n.Alternatives {
			writes = append(writes, e.findWrites(alt.Body, prefix, ports)...)
		}
		if n.Alternative != nil {
			writes = append(writes, e.findWrites(n.Alternative, prefix, ports)...)
		}

	case *ast.WhileStatement:
		writes = append(writes, e.findWrites(n.Body, prefix, ports)...)
	}
	return writes
}
