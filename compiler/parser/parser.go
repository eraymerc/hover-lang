package parser

import (
	"fmt"
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

// Parses type signatures like "unsigned int", "double", or "double[2]"
func (p *parser) parseTypeString() string {
	t := ""
	if p.currentTokenType() == token.UNSIGNED {
		t = "unsigned "
		p.nextToken()
	}
	t += p.currentToken().Literal
	p.nextToken() // base keyword
	for p.currentTokenType() == token.ASTERISK {
		t += "*"
		p.nextToken()
	}
	for p.currentTokenType() == token.LBRACKET {
		p.nextToken()
		t += "[" + p.currentToken().Literal + "]"
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
