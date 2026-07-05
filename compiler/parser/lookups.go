package parser

import (
	ast "hover/compiler/ast"
	token "hover/compiler/token"
)

type binding_power int

const (
	default_bp     binding_power = iota
	logical_or_bp                // ||
	logical_and_bp               // &&
	bit_or_bp                    // |
	bit_xor_bp                   // ^
	bit_and_bp                   // &
	equality_bp                  // ==, !=
	comparison_bp                // >, <, >=, <=
	shift_bp                     // <<, >>
	sum_bp                       // +, -
	product_bp                   // *, /
	power_bp                     // **
	prefix_bp                    // -X, !X, ~X
	call_bp                      // function(), array[], path.
)

type expression_handler func(p *parser) ast.Expression
type left_denoted_handler func(p *parser, left ast.Expression, bp binding_power) ast.Expression

type expression_lookup map[token.Type]expression_handler
type left_denoted_lookup map[token.Type]left_denoted_handler
type binding_power_lookup map[token.Type]binding_power

var bpLookup = binding_power_lookup{
	token.OR:       logical_or_bp,
	token.AND:      logical_and_bp,
	token.BIT_OR:   bit_or_bp,
	token.BIT_XOR:  bit_xor_bp,
	token.BIT_AND:  bit_and_bp,
	token.EQ:       equality_bp,
	token.NOT_EQ:   equality_bp,
	token.LT:       comparison_bp,
	token.GT:       comparison_bp,
	token.LTE:      comparison_bp,
	token.GTE:      comparison_bp,
	token.SHL:      shift_bp,
	token.SHR:      shift_bp,
	token.PLUS:     sum_bp,
	token.MINUS:    sum_bp,
	token.ASTERISK: product_bp,
	token.DIV:      product_bp,
	token.MOD:      product_bp,
	token.POW:      power_bp,
	token.LPAREN:   call_bp,
	token.LBRACKET: call_bp,
	token.DOT:      call_bp,
}

var exprLookup = expression_lookup{}
var leftDenotedLookup = left_denoted_lookup{}

// init() automatically wires the Pratt parsing functions to their tokens
func init() {
	// Prefix Functions
	exprLookup[token.IDENT] = parse_identifier
	exprLookup[token.NUMBER] = parse_number
	exprLookup[token.MINUS] = parse_prefix_expr
	exprLookup[token.BANG] = parse_prefix_expr
	exprLookup[token.BIT_NOT] = parse_prefix_expr
	exprLookup[token.LPAREN] = parse_grouped_expr
	exprLookup[token.LBRACE] = parse_array_literal

	// Infix / Left Denoted Functions
	leftDenotedLookup[token.PLUS] = parse_binary_expr
	leftDenotedLookup[token.MINUS] = parse_binary_expr
	leftDenotedLookup[token.ASTERISK] = parse_binary_expr
	leftDenotedLookup[token.DIV] = parse_binary_expr
	leftDenotedLookup[token.POW] = parse_binary_expr
	leftDenotedLookup[token.MOD] = parse_binary_expr
	leftDenotedLookup[token.EQ] = parse_binary_expr
	leftDenotedLookup[token.NOT_EQ] = parse_binary_expr
	leftDenotedLookup[token.LT] = parse_binary_expr
	leftDenotedLookup[token.GT] = parse_binary_expr
	leftDenotedLookup[token.LTE] = parse_binary_expr
	leftDenotedLookup[token.GTE] = parse_binary_expr
	leftDenotedLookup[token.AND] = parse_binary_expr
	leftDenotedLookup[token.OR] = parse_binary_expr
	leftDenotedLookup[token.BIT_AND] = parse_binary_expr
	leftDenotedLookup[token.BIT_OR] = parse_binary_expr
	leftDenotedLookup[token.BIT_XOR] = parse_binary_expr
	leftDenotedLookup[token.SHL] = parse_binary_expr
	leftDenotedLookup[token.SHR] = parse_binary_expr

	leftDenotedLookup[token.LPAREN] = parse_call_expr
	leftDenotedLookup[token.LBRACKET] = parse_index_expr
	leftDenotedLookup[token.DOT] = parse_binary_expr // Using binary for simple path access a.b
}

func getBindingPower(t token.Type) binding_power {
	if bp, ok := bpLookup[t]; ok {
		return bp
	}
	return default_bp
}
