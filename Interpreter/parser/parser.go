package parser

import (
	"fmt"
	ast "hover/Interpreter/ast"
	token "hover/Interpreter/token"
)

type parser struct {
	tokens []token.Token
	pos    int
	errors []string
}

// Parse is the main entry point
func Parse(tokens []token.Token) *ast.Program {
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

	// Print errors if any were found during parsing
	if len(p.errors) > 0 {
		fmt.Printf("\nParser encountered %d syntax errors:\n", len(p.errors))
		for _, msg := range p.errors {
			fmt.Printf("  - %s\n", msg)
		}
		fmt.Println()
	}

	return program
}

// ==========================================
// UTILITIES & HELPERS
// ==========================================

func (p *parser) currentToken() token.Token    { return p.tokens[p.pos] }
func (p *parser) currentTokenType() token.Type { return p.tokens[p.pos].Type }
func (p *parser) hasTokens() bool              { return p.pos < len(p.tokens) && p.currentTokenType() != token.EOF }

func (p *parser) nextToken() token.Token {
	tok := p.currentToken()
	p.pos++
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
