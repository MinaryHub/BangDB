package sql

import (
	"encoding/hex"
	"fmt"
	"strings"
)

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tQuotedIdent
	tNumber
	tString
	tBlob
	tParam
	tOp
)

type token struct {
	kind tokKind
	text string // upper-cased for tIdent keyword matching is done via upper(text)
	pos  int
}

// SyntaxError reports a parse failure with its byte offset.
type SyntaxError struct {
	Pos int
	Msg string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("bangdb: syntax error at offset %d: %s", e.Pos, e.Msg)
}

func lex(src string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, &SyntaxError{i, "unterminated comment"}
			}
			i += end + 4
		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			word := src[start:i]
			if (word == "x" || word == "X") && i < len(src) && src[i] == '\'' {
				s, n, err := lexString(src, i)
				if err != nil {
					return nil, err
				}
				if _, err := hex.DecodeString(s); err != nil {
					return nil, &SyntaxError{start, "invalid blob literal"}
				}
				toks = append(toks, token{tBlob, s, start})
				i = n
				continue
			}
			toks = append(toks, token{tIdent, word, start})
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			start := i
			for i < len(src) && (src[i] >= '0' && src[i] <= '9' || src[i] == '.') {
				i++
			}
			if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
				j := i + 1
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				if j < len(src) && src[j] >= '0' && src[j] <= '9' {
					i = j
					for i < len(src) && src[i] >= '0' && src[i] <= '9' {
						i++
					}
				}
			}
			toks = append(toks, token{tNumber, src[start:i], start})
		case c == '\'':
			s, n, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tString, s, i})
			i = n
		case c == '"' || c == '`' || c == '[':
			end := byte(c)
			if c == '[' {
				end = ']'
			}
			// A doubled closing quote stands for itself ("a""b").
			var name strings.Builder
			j := i + 1
			for {
				k := strings.IndexByte(src[j:], end)
				if k < 0 {
					return nil, &SyntaxError{i, "unterminated quoted identifier"}
				}
				name.WriteString(src[j : j+k])
				j += k + 1
				if end != ']' && j < len(src) && src[j] == end {
					name.WriteByte(end)
					j++
					continue
				}
				break
			}
			toks = append(toks, token{tQuotedIdent, name.String(), i})
			i = j
		case c == '?' || (c == ':' || c == '@' || c == '$') && i+1 < len(src) && isIdentPart(src[i+1]):
			// ?, ?NNN, $NNN (numbered) and :name, @name, $name (named)
			start := i
			i++
			for i < len(src) && isIdentPart(src[i]) {
				i++
			}
			toks = append(toks, token{tParam, src[start:i], start})
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			if strings.HasPrefix(src[i:], "->>") {
				toks = append(toks, token{tOp, "->>", i})
				i += 3
				continue
			}
			switch two {
			case "<=", ">=", "!=", "<>", "||", "==", "->":
				if two == "<>" {
					two = "!="
				}
				if two == "==" {
					two = "="
				}
				toks = append(toks, token{tOp, two, i})
				i += 2
				continue
			}
			if strings.IndexByte("(),.;*+-/%=<>", c) < 0 {
				return nil, &SyntaxError{i, fmt.Sprintf("unexpected character %q", c)}
			}
			toks = append(toks, token{tOp, string(c), i})
			i++
		}
	}
	toks = append(toks, token{tEOF, "", len(src)})
	return toks, nil
}

func lexString(src string, i int) (string, int, error) {
	var sb strings.Builder
	j := i + 1
	for {
		if j >= len(src) {
			return "", 0, &SyntaxError{i, "unterminated string"}
		}
		if src[j] == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' {
				sb.WriteByte('\'')
				j += 2
				continue
			}
			return sb.String(), j + 1, nil
		}
		sb.WriteByte(src[j])
		j++
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }
