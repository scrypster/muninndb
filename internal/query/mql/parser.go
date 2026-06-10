package mql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Parser implements a recursive descent parser for MQL.
type Parser struct {
	tokens []Token
	pos    int // current token position
	depth  int // recursion depth for parenthesized expressions
}

// NewParser creates a new parser for the given tokens.
func NewParser(tokens []Token) *Parser {
	return &Parser{tokens: tokens}
}

// current returns the current token.
func (p *Parser) current() Token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return Token{Type: TokenEOF}
}

// peek returns the token at pos+1.
func (p *Parser) peek() Token {
	if p.pos+1 < len(p.tokens) {
		return p.tokens[p.pos+1]
	}
	return Token{Type: TokenEOF}
}

// advance moves to the next token.
func (p *Parser) advance() {
	if p.pos < len(p.tokens) {
		p.pos++
	}
}

// expect checks that the current token is of the expected type, then advances.
func (p *Parser) expect(tt TokenType) (Token, error) {
	tok := p.current()
	if tok.Type != tt {
		return tok, p.error(fmt.Sprintf("expected %s, got %s", tt, tok.Type))
	}
	p.advance()
	return tok, nil
}

// error returns a formatted parse error.
func (p *Parser) error(msg string) error {
	tok := p.current()
	return fmt.Errorf("parse error at token %q (pos %d): %s", tok.Value, tok.Pos, msg)
}

// Parse parses the token stream and returns a Query.
// Dispatches to the appropriate query parser based on the first keyword.
// Grammar:
//
//	ACTIVATE FROM <vault> CONTEXT [<term>, ...] [WHERE <predicate>] [MAX_RESULTS <n>] [HOPS <n>] [MIN_RELEVANCE <f>]
//	RECALL EPISODE <episode_id_string> [FRAMES <n>]
//	TRAVERSE FROM <engram_id_string> HOPS <n> [MIN_WEIGHT <f>]
//	CONSOLIDATE VAULT <vault_name> [DRY_RUN]
//	WORKING_MEMORY SESSION <session_id_string>
func (p *Parser) Parse() (Query, error) {
	tok := p.current()

	switch tok.Type {
	case TokenActivate:
		return p.parseActivate()
	case TokenRecall:
		return p.parseRecallEpisode()
	case TokenTraverse:
		return p.parseTraverse()
	case TokenConsolidate:
		return p.parseConsolidate()
	case TokenWorkingMemory:
		return p.parseWorkingMemory()
	default:
		return nil, p.error(fmt.Sprintf("unexpected %q: expected one of: ACTIVATE, RECALL, TRAVERSE, CONSOLIDATE, WORKING_MEMORY", tok.Value))
	}
}

// parseActivate parses an ACTIVATE query.
func (p *Parser) parseActivate() (*ActivateQuery, error) {
	// ACTIVATE
	if _, err := p.expect(TokenActivate); err != nil {
		return nil, err
	}

	// FROM <vault>
	if _, err := p.expect(TokenFrom); err != nil {
		return nil, err
	}
	vaultTok := p.current()
	if vaultTok.Type != TokenIdent && vaultTok.Type != TokenString {
		return nil, p.error("expected vault name")
	}
	vault := vaultTok.Value
	p.advance()

	// CONTEXT [<terms>, ...]
	if _, err := p.expect(TokenContext); err != nil {
		return nil, err
	}

	context, err := p.parseContextList()
	if err != nil {
		return nil, err
	}

	query := &ActivateQuery{
		Vault:   vault,
		Context: context,
	}

	// Parse optional clauses (WHERE, MAX_RESULTS, HOPS, MIN_RELEVANCE)
	for p.current().Type != TokenEOF {
		switch p.current().Type {
		case TokenWhere:
			p.advance()
			predicate, err := p.parsePredicate()
			if err != nil {
				return nil, err
			}
			query.Where = predicate

		case TokenMaxResults:
			p.advance()
			numTok := p.current()
			if numTok.Type != TokenNumber {
				return nil, p.error("expected number after MAX_RESULTS")
			}
			n, err := strconv.Atoi(numTok.Value)
			if err != nil {
				return nil, p.error("invalid number for MAX_RESULTS")
			}
			if n > 1000 {
				n = 1000
			}
			query.MaxResults = n
			p.advance()

		case TokenHops:
			p.advance()
			numTok := p.current()
			if numTok.Type != TokenNumber {
				return nil, p.error("expected number after HOPS")
			}
			n, err := strconv.Atoi(numTok.Value)
			if err != nil {
				return nil, p.error("invalid number for HOPS")
			}
			if n > 10 {
				n = 10
			}
			query.Hops = n
			p.advance()

		case TokenMinRelevance:
			p.advance()
			numTok := p.current()
			if numTok.Type != TokenNumber {
				return nil, p.error("expected number after MIN_RELEVANCE")
			}
			f, err := strconv.ParseFloat(numTok.Value, 32)
			if err != nil {
				return nil, p.error("invalid float for MIN_RELEVANCE")
			}
			query.MinRelevance = float32(f)
			p.advance()

		default:
			return nil, p.error(fmt.Sprintf("unexpected %q in ACTIVATE clause", p.current().Value))
		}
	}

	return query, nil
}

// parseContextList parses CONTEXT [<term1>, <term2>, ...]
func (p *Parser) parseContextList() ([]string, error) {
	if p.current().Type != TokenLBracket {
		return nil, p.error("expected [ after CONTEXT")
	}
	p.advance()

	var context []string
	for p.current().Type != TokenRBracket && p.current().Type != TokenEOF {
		tok := p.current()
		if tok.Type == TokenString || tok.Type == TokenIdent {
			context = append(context, tok.Value)
			p.advance()

			// Optional comma
			if p.current().Type == TokenComma {
				p.advance()
			}
		} else {
			return nil, p.error(fmt.Sprintf("expected string or identifier in context, got %s", tok.Type))
		}
	}

	if _, err := p.expect(TokenRBracket); err != nil {
		return nil, err
	}

	if len(context) == 0 {
		return nil, p.error("context list cannot be empty")
	}

	return context, nil
}

// parsePredicate parses a WHERE predicate with AND/OR support.
// Uses left-associative parsing: expr AND expr OR expr => ((expr AND expr) OR expr)
func (p *Parser) parsePredicate() (Predicate, error) {
	return p.parseOr()
}

// parseOr handles OR predicates.
func (p *Parser) parseOr() (Predicate, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}

	for p.current().Type == TokenOr {
		p.advance()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &OrPredicate{Left: left, Right: right}
	}

	return left, nil
}

// parseAnd handles AND predicates.
func (p *Parser) parseAnd() (Predicate, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}

	for p.current().Type == TokenAnd {
		p.advance()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		left = &AndPredicate{Left: left, Right: right}
	}

	return left, nil
}

// parsePrimary parses a single predicate or a parenthesized expression.
func (p *Parser) parsePrimary() (Predicate, error) {
	tok := p.current()

	// Parenthesized expression
	if tok.Type == TokenLParen {
		p.depth++
		if p.depth > 50 {
			p.depth--
			return nil, p.error("expression nesting too deep (max 50)")
		}
		p.advance()
		pred, err := p.parsePredicate()
		p.depth--
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(TokenRParen); err != nil {
			return nil, err
		}
		return pred, nil
	}

	// state = <state>
	if tok.Type == TokenState {
		p.advance()
		if _, err := p.expect(TokenEQ); err != nil {
			return nil, err
		}
		stateTok := p.current()
		if stateTok.Type != TokenIdent {
			return nil, p.error("expected state name after state =")
		}
		state := strings.ToLower(stateTok.Value)
		p.advance()
		return &StatePredicate{State: state}, nil
	}

	// relevance > <float> or relevance >= <float>
	if tok.Type == TokenRelevance {
		p.advance()
		opTok := p.current()
		if opTok.Type != TokenGT && opTok.Type != TokenGTE {
			return nil, p.error("expected > or >= after relevance")
		}
		op := opTok.Value
		p.advance()

		numTok := p.current()
		if numTok.Type != TokenNumber {
			return nil, p.error("expected number after relevance operator")
		}
		f, err := strconv.ParseFloat(numTok.Value, 32)
		if err != nil {
			return nil, p.error("invalid float for relevance threshold")
		}
		p.advance()
		return &ScorePredicate{Field: "relevance", Op: op, Value: float32(f)}, nil
	}

	// confidence > <float> or confidence >= <float>
	if tok.Type == TokenConfidence {
		p.advance()
		opTok := p.current()
		if opTok.Type != TokenGT && opTok.Type != TokenGTE {
			return nil, p.error("expected > or >= after confidence")
		}
		op := opTok.Value
		p.advance()

		numTok := p.current()
		if numTok.Type != TokenNumber {
			return nil, p.error("expected number after confidence operator")
		}
		f, err := strconv.ParseFloat(numTok.Value, 32)
		if err != nil {
			return nil, p.error("invalid float for confidence threshold")
		}
		p.advance()
		return &ScorePredicate{Field: "confidence", Op: op, Value: float32(f)}, nil
	}

	// tag = "<string>"
	if tok.Type == TokenTag {
		p.advance()
		if _, err := p.expect(TokenEQ); err != nil {
			return nil, err
		}
		tagTok := p.current()
		if tagTok.Type != TokenString {
			return nil, p.error("expected string after tag =")
		}
		tag := tagTok.Value
		p.advance()
		return &TagPredicate{Tag: tag}, nil
	}

	// creator = "<string>"
	if tok.Type == TokenCreator {
		p.advance()
		if _, err := p.expect(TokenEQ); err != nil {
			return nil, err
		}
		creatorTok := p.current()
		if creatorTok.Type != TokenString {
			return nil, p.error("expected string after creator =")
		}
		creator := creatorTok.Value
		p.advance()
		return &CreatorPredicate{Creator: creator}, nil
	}

	// created_after "<RFC3339>"
	if tok.Type == TokenCreatedAfter {
		p.advance()
		timestampTok := p.current()
		if timestampTok.Type != TokenString {
			return nil, p.error("expected RFC3339 string after created_after")
		}
		t, err := time.Parse(time.RFC3339, timestampTok.Value)
		if err != nil {
			return nil, p.error(fmt.Sprintf("invalid RFC3339 timestamp: %v", err))
		}
		p.advance()
		return &CreatedAfterPredicate{After: t}, nil
	}

	// provenance.source = <source_name>
	// provenance.agent = "<agent_id_string>"
	if tok.Type == TokenProvenance {
		p.advance()
		if _, err := p.expect(TokenDot); err != nil {
			return nil, err
		}

		fieldTok := p.current()
		if fieldTok.Type == TokenSource {
			p.advance()
			if _, err := p.expect(TokenEQ); err != nil {
				return nil, err
			}
			sourceTok := p.current()
			if sourceTok.Type != TokenIdent {
				return nil, p.error("expected source name after provenance.source =")
			}
			source := strings.ToLower(sourceTok.Value)
			p.advance()
			return &ProvenanceSourcePredicate{Source: source}, nil
		} else if fieldTok.Type == TokenAgent {
			p.advance()
			if _, err := p.expect(TokenEQ); err != nil {
				return nil, err
			}
			agentTok := p.current()
			if agentTok.Type != TokenString {
				return nil, p.error("expected string after provenance.agent =")
			}
			agent := agentTok.Value
			p.advance()
			return &ProvenanceAgentPredicate{Agent: agent}, nil
		} else {
			return nil, p.error(fmt.Sprintf("expected 'source' or 'agent' after provenance., got %s", fieldTok.Type))
		}
	}

	return nil, p.error(fmt.Sprintf("unexpected %q in predicate", tok.Value))
}

// parseRecallEpisode parses a RECALL EPISODE query.
// Grammar: RECALL EPISODE <episode_id_string> [FRAMES <n>]
func (p *Parser) parseRecallEpisode() (*RecallEpisodeQuery, error) {
	// RECALL
	if _, err := p.expect(TokenRecall); err != nil {
		return nil, err
	}

	// EPISODE
	if _, err := p.expect(TokenEpisode); err != nil {
		return nil, err
	}

	// <episode_id_string>
	idTok := p.current()
	if idTok.Type != TokenString && idTok.Type != TokenIdent {
		return nil, p.error("expected episode ID after EPISODE")
	}
	episodeID := idTok.Value
	p.advance()

	query := &RecallEpisodeQuery{
		EpisodeID: episodeID,
		Frames:    0, // 0 = all frames
	}

	// Optional: FRAMES <n>
	if p.current().Type == TokenFrames {
		p.advance()
		numTok := p.current()
		if numTok.Type != TokenNumber {
			return nil, p.error("expected number after FRAMES")
		}
		n, err := strconv.Atoi(numTok.Value)
		if err != nil {
			return nil, p.error("invalid number for FRAMES")
		}
		query.Frames = n
		p.advance()
	}

	if p.current().Type != TokenEOF {
		return nil, p.error(fmt.Sprintf("unexpected %q after RECALL EPISODE", p.current().Value))
	}

	return query, nil
}

// parseTraverse parses a TRAVERSE query.
// Grammar: TRAVERSE FROM <engram_id_string> HOPS <n> [MIN_WEIGHT <f>]
func (p *Parser) parseTraverse() (*TraverseQuery, error) {
	// TRAVERSE
	if _, err := p.expect(TokenTraverse); err != nil {
		return nil, err
	}

	// FROM
	if _, err := p.expect(TokenFrom); err != nil {
		return nil, err
	}

	// <engram_id_string>
	idTok := p.current()
	if idTok.Type != TokenString && idTok.Type != TokenIdent {
		return nil, p.error("expected engram ID after FROM")
	}
	startID := idTok.Value
	p.advance()

	// HOPS
	if _, err := p.expect(TokenHops); err != nil {
		return nil, err
	}

	// <n>
	numTok := p.current()
	if numTok.Type != TokenNumber {
		return nil, p.error("expected number after HOPS")
	}
	hops, err := strconv.Atoi(numTok.Value)
	if err != nil {
		return nil, p.error("invalid number for HOPS")
	}
	p.advance()

	query := &TraverseQuery{
		StartID:   startID,
		Hops:      hops,
		MinWeight: 0.0, // default
	}

	// Optional: MIN_WEIGHT <f>
	if p.current().Type == TokenMinWeight {
		p.advance()
		floatTok := p.current()
		if floatTok.Type != TokenNumber {
			return nil, p.error("expected number after MIN_WEIGHT")
		}
		f, err := strconv.ParseFloat(floatTok.Value, 32)
		if err != nil {
			return nil, p.error("invalid float for MIN_WEIGHT")
		}
		query.MinWeight = float32(f)
		p.advance()
	}

	if p.current().Type != TokenEOF {
		return nil, p.error(fmt.Sprintf("unexpected %q after TRAVERSE", p.current().Value))
	}

	return query, nil
}

// parseConsolidate parses a CONSOLIDATE query.
// Grammar: CONSOLIDATE VAULT <vault_name> [DRY_RUN]
func (p *Parser) parseConsolidate() (*ConsolidateQuery, error) {
	// CONSOLIDATE
	if _, err := p.expect(TokenConsolidate); err != nil {
		return nil, err
	}

	// VAULT
	if _, err := p.expect(TokenVault); err != nil {
		return nil, err
	}

	// <vault_name>
	vaultTok := p.current()
	if vaultTok.Type != TokenString && vaultTok.Type != TokenIdent {
		return nil, p.error("expected vault name after VAULT")
	}
	vault := vaultTok.Value
	p.advance()

	query := &ConsolidateQuery{
		Vault:  vault,
		DryRun: false,
	}

	// Optional: DRY_RUN
	if p.current().Type == TokenDryRun {
		query.DryRun = true
		p.advance()
	}

	if p.current().Type != TokenEOF {
		return nil, p.error(fmt.Sprintf("unexpected %q after CONSOLIDATE", p.current().Value))
	}

	return query, nil
}

// parseWorkingMemory parses a WORKING_MEMORY query.
// Grammar: WORKING_MEMORY SESSION <session_id_string>
func (p *Parser) parseWorkingMemory() (*WorkingMemoryQuery, error) {
	// WORKING_MEMORY
	if _, err := p.expect(TokenWorkingMemory); err != nil {
		return nil, err
	}

	// SESSION
	if _, err := p.expect(TokenSession); err != nil {
		return nil, err
	}

	// <session_id_string>
	idTok := p.current()
	if idTok.Type != TokenString && idTok.Type != TokenIdent {
		return nil, p.error("expected session ID after SESSION")
	}
	sessionID := idTok.Value
	p.advance()

	if p.current().Type != TokenEOF {
		return nil, p.error(fmt.Sprintf("unexpected %q after WORKING_MEMORY", p.current().Value))
	}

	return &WorkingMemoryQuery{SessionID: sessionID}, nil
}

// Parse is a convenience function that lexes and parses the input string.
func Parse(input string) (Query, error) {
	tokens := Tokenize(input)
	parser := NewParser(tokens)
	return parser.Parse()
}
