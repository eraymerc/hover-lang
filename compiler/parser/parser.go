package parser

import (
	"fmt"
	"strconv"

	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

type parser struct {
	tokens []token.Token
	pos    int
	errors []string
}

// Parse is the main entry point. It returns the parsed program plus every
// syntax error encountered. Callers MUST treat a non-empty error slice as
// fatal: the returned AST is best-effort and may be partial or structurally
// wrong after error recovery, so it must never be fed into semantic
// analysis, elaboration, or codegen.
func Parse(tokens []token.Token) (*ast.Program, []string) {
	p := &parser{
		tokens: tokens,
		pos:    0,
		errors: []string{},
	}

	program := &ast.Program{Statements: []ast.Statement{}}

	for p.hasTokens() {
		startPos := p.pos // WATCHDOG: Record where we started

		stmt := p.parse_statement()
		if stmt != nil {
			program.Statements = append(program.Statements, stmt)
		}

		// PANIC RECOVERY: Force advance if trapped
		if p.pos == startPos {
			p.nextToken()
		}
	}

	return program, p.errors
}

// ==========================================
// UTILITIES & HELPERS
// ==========================================

// currentToken is clamped at the end of the token stream: once the cursor
// has run past the last token, it keeps returning that last token instead
// of panicking with an index-out-of-range. Every token stream main.go
// produces ends in EOF, so "the last token" is always EOF in practice —
// a truncated file (e.g. `double x[` at end-of-file) now surfaces as a
// normal "Expected X, got EOF" syntax error rather than a compiler crash.
func (p *parser) currentToken() token.Token {
	if len(p.tokens) == 0 {
		return token.Token{Type: token.EOF, Literal: "", Line: 1, Column: 1}
	}
	if p.pos >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.pos]
}

func (p *parser) currentTokenType() token.Type { return p.currentToken().Type }

// peekTokenType returns the type of the token n positions ahead without
// moving the cursor, clamped the same way currentToken is: past the end it
// keeps reporting the final token (EOF). Used for the one place the grammar
// needs lookahead — telling a `from ... import ...` statement apart from an
// identifier spelled "from" (see parser/imports.go).
func (p *parser) peekTokenType(n int) token.Type {
	if len(p.tokens) == 0 {
		return token.EOF
	}
	i := p.pos + n
	if i >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1].Type
	}
	return p.tokens[i].Type
}
func (p *parser) hasTokens() bool {
	return p.pos < len(p.tokens) && p.currentTokenType() != token.EOF
}

// nextToken advances the cursor but never past len(tokens): after the final
// token has been consumed, further calls keep returning it (EOF) without
// moving. This cannot re-trap the watchdog loops — every parse loop is
// guarded either by hasTokens() (false once pos reaches the end) or by a
// specific token-type comparison that EOF can never satisfy.
func (p *parser) nextToken() token.Token {
	tok := p.currentToken()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func (p *parser) expect(t token.Type) bool {
	if p.currentTokenType() == t {
		p.nextToken()
		return true
	}
	p.addError(fmt.Sprintf("Expected %s, got %s", t, p.currentTokenType()))
	return false
}

func (p *parser) addError(msg string) {
	p.errors = append(p.errors, fmt.Sprintf("Error at line %d, col %d: %s", p.currentToken().Line, p.currentToken().Column, msg))
}

// parseType parses a type signature like "unsigned int", "double*", or
// "double[2][3]" directly into the structured ast.Type — the single place
// in the whole pipeline where type syntax is parsed. (It used to build a
// string that semantic and codegen each re-parsed with their own shadow
// parsers; see ast/types.go.)
func (p *parser) parseType() ast.Type {
	t := ast.Type{}
	if p.currentTokenType() == token.UNSIGNED {
		p.nextToken()
		t.Base = "unsigned int"
		if p.currentTokenType() == token.INT {
			p.nextToken() // "unsigned int" — consume the int keyword too
		}
	} else {
		t.Base = p.currentToken().Literal
		p.nextToken() // base keyword
	}
	for p.currentTokenType() == token.ASTERISK {
		t.Stars++
		p.nextToken()
	}
	for p.currentTokenType() == token.LBRACKET {
		p.nextToken()
		n, err := strconv.Atoi(p.currentToken().Literal)
		if err != nil || n < 0 {
			// The old string pipeline silently turned a non-numeric
			// dimension into 0 three stages later (codegen's Atoi
			// fallback) — surface it here where the line number is right.
			p.addError(fmt.Sprintf("array dimension must be a non-negative integer literal, got '%s'", p.currentToken().Literal))
			n = 0
		}
		t.Dims = append(t.Dims, n)
		p.nextToken()
		p.expect(token.RBRACKET)
	}
	return t
}

func (p *parser) parseArgList(open token.Type, close token.Type) []ast.Expression {
	args := []ast.Expression{}
	if p.currentTokenType() != open {
		return args // Optional blocks
	}
	p.nextToken() // consume open bracket
	if p.currentTokenType() == close {
		p.nextToken() // consume close bracket
		return args
	}

	// When the closing delimiter is '>', parse expressions at comparison_bp so the
	// Pratt loop condition (bp < getBindingPower(GT)) becomes (comparison_bp < comparison_bp)
	// = false, preventing '>' from being consumed as a comparison operator.
	// For ')' and ']', RPAREN/RBRACKET have no binding power, so default_bp is safe.
	exprBP := default_bp
	if close == token.GT {
		exprBP = comparison_bp
	}

	args = append(args, p.parse_expression(exprBP))
	for p.currentTokenType() == token.COMMA {
		p.nextToken()
		args = append(args, p.parse_expression(exprBP))
	}
	p.expect(close)
	return args
}
