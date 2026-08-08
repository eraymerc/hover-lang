package parser

import (
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

// parseStructDecl parses a top-level struct declaration:
//
//	struct Point { double x; double y; }
//
// Field types reuse parseType() as-is — the same call every other
// declaration site (locals, func params, module args) already uses — so
// nested-struct fields and fixed-array fields work immediately with no
// extra grammar. Unlike a module declaration, there's no trailing ';'
// after the closing brace.
func (p *parser) parseStructDecl() ast.Statement {
	stmt := &ast.StructDeclStatement{Token: p.nextToken()} // consume 'struct'
	stmt.Name = p.currentToken().Literal
	p.expect(token.IDENT)
	p.expect(token.LBRACE)

	for p.currentTokenType() != token.RBRACE && p.hasTokens() {
		f := ast.StructField{}
		f.Type = p.parseType()
		f.Name = p.currentToken().Literal
		p.expect(token.IDENT)
		p.expect(token.SEMI)
		stmt.Fields = append(stmt.Fields, f)
	}
	p.expect(token.RBRACE)
	return stmt
}

// isTypedLocalDeclStart reports whether the cursor starts a bare (non-state)
// local declaration of a named type — `Point p;`, `Point[4] pts;`. The
// disambiguator is pure token SHAPE, not name lookup: the parser has no
// symbol table (by design — name resolution is semantic analysis's job
// throughout this codebase, isPhysicalPrimitive's small closed keyword list
// being the one deliberate exception). IDENT immediately followed by IDENT
// — optionally with pointer stars and/or array dims spliced in between,
// exactly what parseType() itself would consume — never occurs at the start
// of any other Hover statement: assignments start IDENT '=', calls/bare
// expressions start IDENT '(' or an operator, physical primitives are the
// separate fixed keyword set checked by the caller first. This mirrors
// isFromImportStart's contextual-keyword pattern, but keys off shape
// instead of identity, so it works for any struct name without the parser
// needing a registry — exactly like parseType() itself, which accepts any
// Base blindly and lets semantic analysis validate it.
func (p *parser) isTypedLocalDeclStart() bool {
	if p.currentTokenType() != token.IDENT {
		return false
	}
	if isPhysicalPrimitive(p.currentToken().Literal) {
		return false // physical primitives keep priority; caller checks them first anyway
	}
	i := 1
	for p.peekTokenType(i) == token.ASTERISK { // skip pointer stars
		i++
	}
	for p.peekTokenType(i) == token.LBRACKET { // skip one or more [N] dims
		i++ // consume '['
		for p.peekTokenType(i) != token.RBRACKET && p.peekTokenType(i) != token.EOF {
			i++
		}
		i++ // consume ']'
	}
	return p.peekTokenType(i) == token.IDENT
}
