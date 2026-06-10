package mql

import (
	"strings"
	"unicode"
)

// Lexer tokenizes an MQL query string.
type Lexer struct {
	input string
	pos   int  // current position
	ch    rune // current character
}

// NewLexer creates a new lexer for the given input string.
func NewLexer(input string) *Lexer {
	l := &Lexer{input: input}
	l.readChar()
	return l
}

// readChar advances to the next character.
func (l *Lexer) readChar() {
	if l.pos >= len(l.input) {
		l.ch = 0 // EOF
	} else {
		l.ch = rune(l.input[l.pos])
	}
	l.pos++
}

// peekChar returns the next character without advancing.
func (l *Lexer) peekChar() rune {
	if l.pos >= len(l.input) {
		return 0
	}
	return rune(l.input[l.pos])
}

// skipWhitespace skips whitespace and comments.
func (l *Lexer) skipWhitespace() {
	for l.ch != 0 {
		if unicode.IsSpace(l.ch) {
			l.readChar()
		} else if l.ch == '-' && l.peekChar() == '-' {
			// Comment: skip to end of line
			for l.ch != 0 && l.ch != '\n' {
				l.readChar()
			}
			if l.ch == '\n' {
				l.readChar()
			}
		} else {
			break
		}
	}
}

// readString reads a double-quoted string.
func (l *Lexer) readString() string {
	var sb strings.Builder
	l.readChar() // skip opening quote
	for l.ch != 0 && l.ch != '"' {
		if l.ch == '\\' {
			l.readChar()
			// Simple escape handling: \n, \t, \", \\
			switch l.ch {
			case 'n':
				sb.WriteRune('\n')
			case 't':
				sb.WriteRune('\t')
			case '"':
				sb.WriteRune('"')
			case '\\':
				sb.WriteRune('\\')
			default:
				sb.WriteRune(l.ch)
			}
			l.readChar()
		} else {
			sb.WriteRune(l.ch)
			l.readChar()
		}
	}
	if l.ch == '"' {
		l.readChar() // skip closing quote
	}
	return sb.String()
}

// readNumber reads an integer or float.
func (l *Lexer) readNumber() string {
	var sb strings.Builder
	for unicode.IsDigit(l.ch) {
		sb.WriteRune(l.ch)
		l.readChar()
	}
	if l.ch == '.' && unicode.IsDigit(l.peekChar()) {
		sb.WriteRune(l.ch)
		l.readChar()
		for unicode.IsDigit(l.ch) {
			sb.WriteRune(l.ch)
			l.readChar()
		}
	}
	return sb.String()
}

// readIdent reads an identifier or keyword.
func (l *Lexer) readIdent() string {
	var sb strings.Builder
	for unicode.IsLetter(l.ch) || unicode.IsDigit(l.ch) || l.ch == '_' {
		sb.WriteRune(l.ch)
		l.readChar()
	}
	return sb.String()
}

// lookupKeyword returns the token type for a keyword, or TokenIdent if not a keyword.
func lookupKeyword(ident string) TokenType {
	keywords := map[string]TokenType{
		"ACTIVATE":       TokenActivate,
		"FROM":           TokenFrom,
		"CONTEXT":        TokenContext,
		"WHERE":          TokenWhere,
		"MAX_RESULTS":    TokenMaxResults,
		"HOPS":           TokenHops,
		"MIN_RELEVANCE":  TokenMinRelevance,
		"AND":            TokenAnd,
		"OR":             TokenOr,
		"STATE":          TokenState,
		"RELEVANCE":      TokenRelevance,
		"CONFIDENCE":     TokenConfidence,
		"TAG":            TokenTag,
		"CREATOR":        TokenCreator,
		"CREATED_AFTER":  TokenCreatedAfter,
		"RECALL":         TokenRecall,
		"EPISODE":        TokenEpisode,
		"FRAMES":         TokenFrames,
		"TRAVERSE":       TokenTraverse,
		"CONSOLIDATE":    TokenConsolidate,
		"VAULT":          TokenVault,
		"WORKING_MEMORY": TokenWorkingMemory,
		"SESSION":        TokenSession,
		"DRY_RUN":        TokenDryRun,
		"PROVENANCE":     TokenProvenance,
		"SOURCE":         TokenSource,
		"AGENT":          TokenAgent,
		"MIN_WEIGHT":     TokenMinWeight,
	}
	// Case-insensitive lookup
	upperIdent := strings.ToUpper(ident)
	if tt, ok := keywords[upperIdent]; ok {
		return tt
	}
	return TokenIdent
}

// NextToken returns the next token from the input.
func (l *Lexer) NextToken() Token {
	l.skipWhitespace()

	start := l.pos - 1
	var tt TokenType
	var value string

	switch l.ch {
	case 0:
		tt, value = TokenEOF, ""
	case '=':
		tt, value = TokenEQ, "="
		l.readChar()
	case '>':
		if l.peekChar() == '=' {
			tt, value = TokenGTE, ">="
			l.readChar()
			l.readChar()
		} else {
			tt, value = TokenGT, ">"
			l.readChar()
		}
	case '(':
		tt, value = TokenLParen, "("
		l.readChar()
	case ')':
		tt, value = TokenRParen, ")"
		l.readChar()
	case '[':
		tt, value = TokenLBracket, "["
		l.readChar()
	case ']':
		tt, value = TokenRBracket, "]"
		l.readChar()
	case ',':
		tt, value = TokenComma, ","
		l.readChar()
	case '.':
		tt, value = TokenDot, "."
		l.readChar()
	case '"':
		value = l.readString()
		tt = TokenString
	default:
		if unicode.IsDigit(l.ch) {
			value = l.readNumber()
			tt = TokenNumber
		} else if unicode.IsLetter(l.ch) || l.ch == '_' {
			value = l.readIdent()
			tt = lookupKeyword(value)
		} else {
			// Unknown character, skip it
			l.readChar()
			tt = TokenIdent
			value = ""
		}
	}

	return Token{
		Type:  tt,
		Value: value,
		Pos:   start,
	}
}

// Tokenize returns all tokens from the input until EOF.
func Tokenize(input string) []Token {
	lexer := NewLexer(input)
	var tokens []Token
	for {
		tok := lexer.NextToken()
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	return tokens
}
