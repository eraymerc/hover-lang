package parser

import (
	ast "hover/Interpreter/ast"
	token "hover/Interpreter/token"
)

// ==========================================
// MASTER ROUTER
// ==========================================

func (p *parser) parse_statement() ast.Statement {
	switch p.currentTokenType() {
	case token.MODULE, token.ANALOG, token.DIGITAL:
		return p.parseModuleStatement()
	case token.IMPORT:
		return p.parseImportStatement()
	case token.IMPORTC:
		return p.parseImportCStatement()
	case token.EXTERN:
		return p.parseExternFuncDecl()
	case token.FUNC:
		return p.parseFuncDecl()
	case token.STATE, token.DOUBLE, token.INT, token.FLOAT, token.UNSIGNED, token.WIRE:
		return p.parseLocalDecl()
	case token.IF:
		return p.parseIfStatement()
	case token.WHILE:
		return p.parseWhileStatement()
	case token.RETURN:
		return p.parseReturnStatement()
	case token.DOT:
		return p.parseDirective()
	case token.LBRACE:
		return p.parseBlockStatement()
	default:
		// Could be a Physical Primitive (R, C, L) or an Assignment (x = 5;)
		if p.currentTokenType() == token.IDENT && isPhysicalPrimitive(p.currentToken().Literal) {
			return p.parsePhysicalPrimitive()
		}
		return p.parseExpressionOrAssignment()
	}
}

// ==========================================
// STATEMENT PARSERS
// ==========================================

func (p *parser) parseExpressionOrAssignment() ast.Statement {
	startTok := p.currentToken()

	// Parse the left side first
	expr := p.parse_expression(default_bp)

	// Bail out on failure
	if expr == nil {
		return nil
	}

	// If the next token is '=', it's an assignment
	if p.currentTokenType() == token.ASSIGN {
		assignTok := p.nextToken() // Consume '='
		right := p.parse_expression(default_bp)
		p.expect(token.SEMI)
		return &ast.AssignmentStatement{Token: assignTok, Left: expr, Right: right}
	}

	// Otherwise, it's just a standalone expression
	p.expect(token.SEMI)
	return &ast.ExpressionStatement{Token: startTok, Expression: expr}
}

func (p *parser) parseLocalDecl() ast.Statement {
	stmt := &ast.LocalDeclStatement{Token: p.currentToken()}

	if p.currentTokenType() == token.STATE {
		stmt.IsState = true
		p.nextToken()
	}

	stmt.Type = p.parseTypeString()

	for {
		decl := &ast.VarDecl{}
		decl.Name = p.currentToken().Literal
		p.expect(token.IDENT)

		if p.currentTokenType() == token.ASSIGN {
			p.nextToken() // consume '='
			decl.Value = p.parse_expression(default_bp)
		}
		stmt.Decls = append(stmt.Decls, decl)

		if p.currentTokenType() == token.COMMA {
			p.nextToken()
		} else {
			break
		}
	}
	p.expect(token.SEMI)
	return stmt
}

func (p *parser) parseBlockStatement() *ast.BlockStatement {
	block := &ast.BlockStatement{Token: p.currentToken()}
	p.expect(token.LBRACE)

	for p.currentTokenType() != token.RBRACE && p.hasTokens() {
		startPos := p.pos // WATCHDOG for inner blocks

		stmt := p.parse_statement()
		if stmt != nil {
			block.Body = append(block.Body, stmt)
		}

		if p.pos == startPos {
			p.nextToken() // Prevent infinite loop inside block
		}
	}
	p.expect(token.RBRACE)
	return block
}

func (p *parser) parseIfStatement() ast.Statement {
	stmt := &ast.IfStatement{Token: p.nextToken()} // consume 'if'

	p.expect(token.LPAREN)
	stmt.Condition = p.parse_expression(default_bp)
	p.expect(token.RPAREN)

	stmt.Consequence = p.parseBlockStatement()

	// Handle explicit 'else if' and 'else' chaining
	for p.currentTokenType() == token.ELSE {
		elseToken := p.nextToken() // consume 'else'

		if p.currentTokenType() == token.IF {
			p.nextToken() // consume 'if'

			p.expect(token.LPAREN)
			condition := p.parse_expression(default_bp)
			p.expect(token.RPAREN)

			body := p.parseBlockStatement()

			stmt.Alternatives = append(stmt.Alternatives, &ast.ElseIfBlock{
				Token:     elseToken,
				Condition: condition,
				Body:      body,
			})
		} else {
			// Final 'else' block
			stmt.Alternative = p.parseBlockStatement()
			break // Must be the end of the if-else chain
		}
	}

	return stmt
}

func (p *parser) parseWhileStatement() ast.Statement {
	stmt := &ast.WhileStatement{Token: p.nextToken()}
	p.expect(token.LPAREN)
	stmt.Condition = p.parse_expression(default_bp)
	p.expect(token.RPAREN)
	stmt.Body = p.parseBlockStatement()
	return stmt
}

func (p *parser) parseReturnStatement() ast.Statement {
	stmt := &ast.ReturnStatement{Token: p.nextToken()}
	stmt.ReturnValue = p.parse_expression(default_bp)
	p.expect(token.SEMI)
	return stmt
}

func (p *parser) parseImportCStatement() ast.Statement {
	stmt := &ast.ImportCStatement{Token: p.nextToken()} // consume 'importc'
	stmt.Path = p.currentToken().Literal
	p.expect(token.STRING)
	if p.currentTokenType() == token.SEMI { // trailing ';' optional
		p.nextToken()
	}
	return stmt
}

func (p *parser) parseExternFuncDecl() ast.Statement {
	externTok := p.nextToken() // consume 'extern'
	if p.currentTokenType() != token.FUNC {
		p.addError("expected 'func' after 'extern'")
		return nil
	}
	p.nextToken() // consume 'func'

	stmt := &ast.FuncDeclStatement{Token: externTok, IsExtern: true}
	stmt.ReturnType = p.parseTypeString()
	stmt.Name = p.currentToken().Literal
	p.expect(token.IDENT)

	p.expect(token.LPAREN)
	for p.currentTokenType() != token.RPAREN && p.hasTokens() {
		param := ast.FuncParam{}
		param.Type = p.parseTypeString()
		if p.currentTokenType() == token.IDENT { // param name optional (C-style)
			param.Name = p.currentToken().Literal
			p.nextToken()
		}
		stmt.Parameters = append(stmt.Parameters, param)
		if p.currentTokenType() == token.COMMA {
			p.nextToken()
		}
	}
	p.expect(token.RPAREN)
	if p.currentTokenType() == token.SEMI { // trailing ';' optional
		p.nextToken()
	}
	return stmt // Body stays nil
}
