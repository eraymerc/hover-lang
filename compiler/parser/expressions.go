package parser

import (
	"fmt"
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

// THE PRATT LOOP
func (p *parser) parse_expression(bp binding_power) ast.Expression {
	tok := p.currentToken()

	prefixFn := exprLookup[tok.Type]
	if prefixFn == nil {
		p.addError(fmt.Sprintf("No prefix parse function for %s found", tok.Literal))
		return nil
	}

	leftExp := prefixFn(p)

	for p.hasTokens() && bp < getBindingPower(p.currentTokenType()) {
		infixFn := leftDenotedLookup[p.currentTokenType()]
		if infixFn == nil {
			return leftExp
		}
		leftExp = infixFn(p, leftExp, getBindingPower(p.currentTokenType()))
	}

	return leftExp
}

// ==========================================
// EXPRESSION PARSERS (PREFIX)
// ==========================================

func parse_identifier(p *parser) ast.Expression {
	tok := p.nextToken()
	return &ast.IdentifierExpression{Token: tok, Value: tok.Literal}
}

func parse_number(p *parser) ast.Expression {
	tok := p.nextToken()
	return &ast.NumberExpression{Token: tok, Value: tok.Literal}
}

func parse_prefix_expr(p *parser) ast.Expression {
	expr := &ast.UnaryExpression{Token: p.currentToken(), Operator: p.currentToken().Literal}
	p.nextToken()
	expr.Right = p.parse_expression(prefix_bp)
	return expr
}

func parse_grouped_expr(p *parser) ast.Expression {
	p.nextToken() // consume '('
	exp := p.parse_expression(default_bp)
	p.expect(token.RPAREN)
	return exp
}

func parse_array_literal(p *parser) ast.Expression {
	expr := &ast.ArrayExpression{Token: p.nextToken()} // consume '{'
	if p.currentTokenType() != token.RBRACE {
		expr.Elements = append(expr.Elements, p.parse_expression(default_bp))
		for p.currentTokenType() == token.COMMA {
			p.nextToken()
			expr.Elements = append(expr.Elements, p.parse_expression(default_bp))
		}
	}
	p.expect(token.RBRACE)
	return expr
}

// ==========================================
// EXPRESSION PARSERS (INFIX)
// ==========================================

func parse_binary_expr(p *parser, left ast.Expression, bp binding_power) ast.Expression {
	expr := &ast.BinaryExpression{Token: p.currentToken(), Operator: p.currentToken().Literal, Left: left}
	p.nextToken()
	expr.Right = p.parse_expression(bp)
	return expr
}

func parse_call_expr(p *parser, left ast.Expression, bp binding_power) ast.Expression {
	expr := &ast.CallExpression{Token: p.currentToken(), Function: left}
	expr.Arguments = p.parseArgList(token.LPAREN, token.RPAREN)
	return expr
}

func parse_index_expr(p *parser, left ast.Expression, bp binding_power) ast.Expression {
	expr := &ast.IndexExpression{Token: p.nextToken(), Left: left} // consume '['
	expr.Index = p.parse_expression(default_bp)
	p.expect(token.RBRACKET)
	return expr
}

// parse_struct_literal parses TypeName{field: expr, ...} — a struct literal.
// Registered as the LBRACE infix handler (lookups.go), so it only fires
// when '{' immediately follows another expression; today that shape is
// always a syntax error (no prior infix handler exists for LBRACE), so this
// is purely additive and cannot change the parse of any program that
// compiled before. `left` must be a bare type-name identifier — struct
// literals are always written TypeName{...}, never as a chained postfix on
// an arbitrary expression.
func parse_struct_literal(p *parser, left ast.Expression, bp binding_power) ast.Expression {
	id, ok := left.(*ast.IdentifierExpression)
	if !ok {
		p.addError("struct literal must be prefixed by a type name")
		return left
	}
	expr := &ast.StructLiteralExpression{Token: p.nextToken(), TypeName: id.Value} // consume '{'
	if p.currentTokenType() != token.RBRACE {
		for {
			fname := p.currentToken().Literal
			p.expect(token.IDENT)
			p.expect(token.COLON)
			val := p.parse_expression(default_bp)
			expr.Fields = append(expr.Fields, ast.StructFieldInit{Name: fname, Value: val})
			if p.currentTokenType() == token.COMMA {
				p.nextToken()
				continue
			}
			break
		}
	}
	p.expect(token.RBRACE)
	return expr
}
