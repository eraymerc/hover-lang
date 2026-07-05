package parser

import (
	ast "hover/compiler/ast"
	token "hover/compiler/token"
	"strings"
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
	stmt := &ast.ImportStatement{Token: p.nextToken()}
	if p.currentTokenType() == token.LT {
		stmt.IsSystem = true
		p.nextToken() // '<'
		var b strings.Builder
		for p.currentTokenType() != token.GT && p.currentTokenType() != token.EOF {
			b.WriteString(p.currentToken().Literal)
			p.nextToken()
		}
		stmt.Path = b.String() // "math/math.hvr", bracket-free
		p.expect(token.GT)
	} else {
		stmt.Path = p.currentToken().Literal
		p.expect(token.STRING)
	}
	if p.currentTokenType() == token.AS {
		p.nextToken()
		stmt.Alias = p.currentToken().Literal
		p.expect(token.IDENT)
	}
	p.expect(token.SEMI)
	return stmt
}

// readAnglePath consumes `< path/to/file.hvr >` and returns the path with the
// delimiters stripped. The path is rebuilt by concatenating the literal text
// of every token between the brackets — "semiconductors" + "/" + "npn_bjt" +
// "." + "hvr" — which works because the lexer emits each of '/', '.', '_' with
// its own character as the token literal (the newToken(type, l.ch) pattern).
// Whitespace inside the brackets is dropped, matching C's <...> behaviour.
func (p *parser) readAnglePath() string {
	p.expect(token.LT) // consume '<'
	var b strings.Builder
	for p.currentTokenType() != token.GT && p.currentTokenType() != token.EOF {
		b.WriteString(p.currentToken().Literal)
		p.nextToken()
	}
	p.expect(token.GT) // consume '>'
	return b.String()
}
