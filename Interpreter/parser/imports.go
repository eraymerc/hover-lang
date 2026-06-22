package parser

import (
	ast "hover/Interpreter/ast"
	token "hover/Interpreter/token"
)

// ==========================================
// IMPORTS
// ==========================================

// parseImportStatement parses:
//
//	import "path.hvr";
//	import "path.hvr" as Alias;
//
// The path is taken directly from a STRING token's Literal — no expression
// parsing involved, since import paths are always a single string literal,
// never a computed value.
func (p *parser) parseImportStatement() ast.Statement {
	stmt := &ast.ImportStatement{Token: p.nextToken()} // consume 'import'

	stmt.Path = p.currentToken().Literal
	p.expect(token.STRING)

	if p.currentTokenType() == token.AS {
		p.nextToken() // consume 'as'
		stmt.Alias = p.currentToken().Literal
		p.expect(token.IDENT)
	}

	p.expect(token.SEMI)
	return stmt
}
