package parser

import (
	"fmt"
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

// ==========================================
// HVR SPECIFIC (MODULES & PRIMITIVES)
// ==========================================

func (p *parser) parseModuleStatement() ast.Statement {
	// 1. CAPTURE THE DOMAIN (Will be ANALOG, DIGITAL, or MODULE)
	domainToken := p.currentToken()

	// 2. CONSUME THE PREFIX (if it exists) AND EXPECT 'module'
	if p.currentTokenType() == token.ANALOG || p.currentTokenType() == token.DIGITAL {
		p.nextToken() // move past 'analog' or 'digital'
		if p.currentTokenType() != token.MODULE {
			p.errors = append(p.errors, fmt.Sprintf("Error at line %d: expected 'module' after prefix", p.currentToken().Line))
			return nil
		}
	}

	p.nextToken() // consume 'module'
	name := p.currentToken().Literal
	p.expect(token.IDENT)

	if p.currentTokenType() == token.ASSIGN {
		// INSTANTIATION: module my_ctrl = PI <...>(...)[...];
		p.nextToken() // consume '='
		modName := p.currentToken().Literal
		p.expect(token.IDENT)
		// Support qualified names from aliased imports: Alias.ModuleName.
		// Only one dot is meaningful — imports are non-transitive, so a
		// module name is always either bare ("PMSM") or exactly one alias
		// segment plus the real name ("Motor.PMSM"), never deeper.
		if p.currentTokenType() == token.DOT {
			p.nextToken() // consume '.'
			modName = modName + "." + p.currentToken().Literal
			p.expect(token.IDENT)
		}

		stmt := &ast.ModuleInstStatement{Token: domainToken, InstanceName: name, ModuleName: modName}
		stmt.StaticArgs = p.parseArgList(token.LT, token.GT)
		stmt.LogicArgs = p.parseArgList(token.LPAREN, token.RPAREN)
		stmt.PhysArgs = p.parseArgList(token.LBRACKET, token.RBRACKET)
		p.expect(token.SEMI)
		return stmt
	}

	// DECLARATION: module PI <double param> (input double sig) [wire port] { ... }
	stmt := &ast.ModuleDeclStatement{Token: domainToken, Name: name}

	// 1. Parse Static Arguments <type name [= value], ...>
	if p.currentTokenType() == token.LT {
		p.nextToken()
		for p.currentTokenType() != token.GT && p.hasTokens() {
			arg := ast.DeclStaticArg{}
			arg.Type = p.parseType()
			arg.Name = p.currentToken().Literal
			p.expect(token.IDENT)
			if p.currentTokenType() == token.ASSIGN {
				p.nextToken()
				arg.Value = p.parse_expression(default_bp)
			}
			stmt.StaticArgs = append(stmt.StaticArgs, arg)
			if p.currentTokenType() == token.COMMA {
				p.nextToken()
			}
		}
		p.expect(token.GT)
	}

	// 2. Parse Logic Arguments (direction type name, ...)
	if p.currentTokenType() == token.LPAREN {
		p.nextToken()
		for p.currentTokenType() != token.RPAREN && p.hasTokens() {
			arg := ast.DeclLogicArg{}
			if p.currentToken().Literal == "input" || p.currentToken().Literal == "output" {
				arg.Direction = p.currentToken().Literal
				p.nextToken()
			}
			arg.Type = p.parseType()
			arg.Name = p.currentToken().Literal
			p.expect(token.IDENT)
			stmt.LogicArgs = append(stmt.LogicArgs, arg)
			if p.currentTokenType() == token.COMMA {
				p.nextToken()
			}
		}
		p.expect(token.RPAREN)
	}

	// 3. Parse Physical Ports [wire name, ...]
	if p.currentTokenType() == token.LBRACKET {
		p.nextToken()
		for p.currentTokenType() != token.RBRACKET && p.hasTokens() {
			startPos := p.pos
			if p.currentToken().Literal == "wire" {
				p.nextToken() // consume 'wire' if present
			}
			portName := p.currentToken().Literal
			p.expect(token.IDENT)
			stmt.PhysPorts = append(stmt.PhysPorts, portName)
			if p.currentTokenType() == token.COMMA {
				p.nextToken()
			}
			if p.pos == startPos {
				p.addError("unexpected token in port list: " + p.currentToken().Literal)
				p.nextToken() // force progress
			}
		}
		p.expect(token.RBRACKET)
	}

	stmt.Body = p.parseBlockStatement()
	return stmt
}

func (p *parser) parsePhysicalPrimitive() ast.Statement {
	stmt := &ast.PhysicalPrimitiveStatement{Token: p.currentToken(), PrimType: p.currentToken().Literal}
	p.nextToken() // consume prim name (e.g. 'R')

	// Optional instance name: `R rsense<1m>() [b, s];`
	//
	// Unambiguous by construction — the only tokens the grammar ever allowed
	// in this position were '<', '(', '[' or ';', never an IDENT, so an IDENT
	// here can only be a name and every existing unnamed form still parses
	// exactly as before. No validation here: the parser has no scope, so
	// duplicates and collisions are semantic/elaborator business.
	if p.currentTokenType() == token.IDENT {
		stmt.Name = p.currentToken().Literal
		p.nextToken()
	}

	stmt.StaticArgs = p.parseArgList(token.LT, token.GT)
	stmt.LogicArgs = p.parseArgList(token.LPAREN, token.RPAREN)
	stmt.PhysArgs = p.parseArgList(token.LBRACKET, token.RBRACKET)
	p.expect(token.SEMI)
	return stmt
}

func (p *parser) parseFuncDecl() ast.Statement {
	stmt := &ast.FuncDeclStatement{Token: p.nextToken()}
	stmt.ReturnType = p.parseType()
	stmt.Name = p.currentToken().Literal
	p.expect(token.IDENT)

	p.expect(token.LPAREN)
	for p.currentTokenType() != token.RPAREN && p.hasTokens() {
		param := ast.FuncParam{}
		param.Type = p.parseType()
		param.Name = p.currentToken().Literal
		p.expect(token.IDENT)
		stmt.Parameters = append(stmt.Parameters, param)
		if p.currentTokenType() == token.COMMA {
			p.nextToken()
		}
	}
	p.expect(token.RPAREN)
	stmt.Body = p.parseBlockStatement()
	return stmt
}

func (p *parser) parseDirective() ast.Statement {
	stmt := &ast.DirectiveStatement{Token: p.nextToken()} // Consume '.'
	stmt.Name = p.currentToken().Literal
	p.expect(token.IDENT)
	stmt.Args = p.parseArgList(token.LPAREN, token.RPAREN)
	p.expect(token.SEMI)
	return stmt
}

func isPhysicalPrimitive(name string) bool {
	switch name {
	case "L", "C", "R", "VCVS", "VCCS", "CCVS", "CCCS", "current_source", "voltage_source":
		return true
	}
	return false
}
