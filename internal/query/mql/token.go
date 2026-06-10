package mql

import "fmt"

// TokenType represents the type of a token.
type TokenType string

// Token types for the MQL lexer.
const (
	// Keywords
	TokenActivate      TokenType = "ACTIVATE"
	TokenFrom          TokenType = "FROM"
	TokenContext       TokenType = "CONTEXT"
	TokenWhere         TokenType = "WHERE"
	TokenMaxResults    TokenType = "MAX_RESULTS"
	TokenHops          TokenType = "HOPS"
	TokenMinRelevance  TokenType = "MIN_RELEVANCE"
	TokenAnd           TokenType = "AND"
	TokenOr            TokenType = "OR"
	TokenState         TokenType = "STATE"
	TokenRelevance     TokenType = "RELEVANCE"
	TokenConfidence    TokenType = "CONFIDENCE"
	TokenTag           TokenType = "TAG"
	TokenCreator       TokenType = "CREATOR"
	TokenCreatedAfter  TokenType = "CREATED_AFTER"
	TokenRecall        TokenType = "RECALL"
	TokenEpisode       TokenType = "EPISODE"
	TokenFrames        TokenType = "FRAMES"
	TokenTraverse      TokenType = "TRAVERSE"
	TokenConsolidate   TokenType = "CONSOLIDATE"
	TokenVault         TokenType = "VAULT"
	TokenWorkingMemory TokenType = "WORKING_MEMORY"
	TokenSession       TokenType = "SESSION"
	TokenDryRun        TokenType = "DRY_RUN"
	TokenProvenance    TokenType = "PROVENANCE"
	TokenSource        TokenType = "SOURCE"
	TokenAgent         TokenType = "AGENT"
	TokenMinWeight     TokenType = "MIN_WEIGHT"
	TokenDot           TokenType = "."

	// Operators
	TokenEQ  TokenType = "="
	TokenGT  TokenType = ">"
	TokenGTE TokenType = ">="

	// Delimiters
	TokenLParen   TokenType = "("
	TokenRParen   TokenType = ")"
	TokenLBracket TokenType = "["
	TokenRBracket TokenType = "]"
	TokenComma    TokenType = ","

	// Literals
	TokenString TokenType = "STRING"
	TokenNumber TokenType = "NUMBER"
	TokenIdent  TokenType = "IDENT"

	// Special
	TokenEOF TokenType = "EOF"
)

// Token represents a single lexical token.
type Token struct {
	Type  TokenType
	Value string
	Pos   int // byte position in input
}

// String returns a human-readable representation of the token.
func (t Token) String() string {
	return fmt.Sprintf("{%s %q @%d}", t.Type, t.Value, t.Pos)
}
