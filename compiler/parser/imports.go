package parser

import (
	ast "hover/compiler/ast"
	token "hover/compiler/token"
	"strings"
)

// ==========================================
// IMPORTS
// ==========================================

// parseImportStatement parses the whole-file forms:
//
//	import "path.hvr";
//	import "path.hvr" as Alias;
//	import <stdlib/path.hvr>;
//
// The path is taken directly from a STRING token's Literal — no expression
// parsing involved, since import paths are always a single string literal,
// never a computed value.
func (p *parser) parseImportStatement() ast.Statement {
	stmt := &ast.ImportStatement{Token: p.nextToken()}
	p.parseImportPath(stmt)
	if p.currentTokenType() == token.AS {
		p.nextToken()
		stmt.Alias = p.currentToken().Literal
		p.expect(token.IDENT)
	}
	p.expect(token.SEMI)
	return stmt
}

// isFromImportStart reports whether the cursor sits on a `from` that begins
// an import statement, as opposed to an identifier that merely happens to be
// spelled "from". The disambiguator is the token after it: an import path is
// either a string literal or an angle-bracketed path, and neither can follow
// an identifier in any other statement form (`from = 3;`, `from + 1`,
// `from.x`, `R from(a, b);` all continue with something else).
func (p *parser) isFromImportStart() bool {
	if p.currentTokenType() != token.IDENT || p.currentToken().Literal != "from" {
		return false
	}
	next := p.peekTokenType(1)
	return next == token.STRING || next == token.LT
}

// parseFromImportStatement parses the selective forms:
//
//	from <math/math.hvr> import sin;
//	from <math/math.hvr> import sin as s, cos;
//	from "./devices.hvr" import LED;
//
// Only the listed names become visible, each bound under its own name or
// under the local name given by `as`. This is a different meaning of `as`
// from the whole-file form — there it namespaces an entire file, here it
// renames one declaration — which is why the two are mutually exclusive.
func (p *parser) parseFromImportStatement() ast.Statement {
	stmt := &ast.ImportStatement{Token: p.nextToken(), Selective: true} // consume 'from'
	p.parseImportPath(stmt)

	if p.currentTokenType() != token.IMPORT {
		p.addError("Expected 'import' after the path in a `from ... import ...` statement")
		p.skipToSemicolon()
		return stmt
	}
	p.nextToken() // consume 'import'

	for {
		name := p.currentToken().Literal
		if !p.expect(token.IDENT) {
			p.skipToSemicolon()
			return stmt
		}
		sym := ast.ImportedSymbol{Name: name}
		if p.currentTokenType() == token.AS {
			p.nextToken()
			sym.Alias = p.currentToken().Literal
			if !p.expect(token.IDENT) {
				p.skipToSemicolon()
				return stmt
			}
		}
		stmt.Names = append(stmt.Names, sym)

		if p.currentTokenType() != token.COMMA {
			break
		}
		p.nextToken() // consume ','
	}

	if len(stmt.Names) == 0 {
		p.addError("`from " + stmt.PathString() + " import` needs at least one name")
	}
	p.expect(token.SEMI)
	return stmt
}

// parseImportPath consumes the path of either import form — `"quoted"` or
// `<angled>` — and records it on stmt. Shared so the two statement forms can
// never drift apart in what a path is allowed to look like, which matters
// now that a path may also name a package (`<@pkg/file.hvr>`).
func (p *parser) parseImportPath(stmt *ast.ImportStatement) {
	if p.currentTokenType() == token.LT {
		stmt.IsSystem = true
		stmt.Path = p.readAnglePath()
		return
	}
	stmt.Path = p.currentToken().Literal
	p.expect(token.STRING)
}

// skipToSemicolon discards tokens up to and including the next ';' so that
// one malformed import produces one error rather than a cascade of them from
// the parser trying to read its remains as a new statement.
func (p *parser) skipToSemicolon() {
	for p.currentTokenType() != token.SEMI && p.currentTokenType() != token.EOF {
		p.nextToken()
	}
	if p.currentTokenType() == token.SEMI {
		p.nextToken()
	}
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
