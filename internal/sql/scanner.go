package sql

import (
	"bufio"
	"io"
	"strings"
	"unicode"
)

// Scanner splits a stream of SQL text into statements, so that large
// scripts (dumps) can be run without reading them whole. Semicolons inside
// string literals, quoted identifiers and comments do not end a statement,
// nor do those inside the BEGIN ... END body of a CREATE TRIGGER.
type Scanner struct {
	r         *bufio.Reader
	line      int // line of the next character, from 1
	startLine int // line where the last statement returned by Next starts
}

// NewScanner returns a Scanner reading from r.
func NewScanner(r io.Reader) *Scanner { return &Scanner{r: bufio.NewReaderSize(r, 64<<10), line: 1} }

// Line returns the line on which the statement last returned by Next
// starts (its first character that is not a blank or in a comment).
func (s *Scanner) Line() int { return s.startLine }

// Next returns the next statement without its terminating semicolon, or
// io.EOF when no statement is left. Comments are kept; a statement that
// consists only of comments and blanks is skipped.
func (s *Scanner) Next() (string, error) {
	var b strings.Builder
	var words []string // first words of the statement (upper case)
	var word strings.Builder
	lastWord := ""
	endWord := func() {
		if word.Len() == 0 {
			return
		}
		w := strings.ToUpper(word.String())
		if len(words) < 4 {
			words = append(words, w)
		}
		lastWord = w
		word.Reset()
	}
	isTrigger := func() bool {
		if len(words) < 2 || words[0] != "CREATE" {
			return false
		}
		w := words[1]
		if (w == "TEMP" || w == "TEMPORARY") && len(words) > 2 {
			w = words[2]
		}
		return w == "TRIGGER"
	}
	content := false // anything but blanks and comments seen
	seen := func() {
		if !content {
			content, s.startLine = true, s.line
		}
	}
	for {
		c, _, err := s.r.ReadRune()
		if err == io.EOF {
			endWord()
			if !content {
				return "", io.EOF
			}
			return strings.TrimSpace(b.String()), nil
		}
		if err != nil {
			return "", err
		}
		if c == '\n' {
			s.line++
		}
		switch {
		case c == ';':
			endWord()
			if isTrigger() && lastWord != "END" {
				b.WriteRune(c)
				lastWord = ""
				continue
			}
			if !content {
				b.Reset()
				words, lastWord = words[:0], ""
				continue
			}
			return strings.TrimSpace(b.String()), nil
		case c == '\'' || c == '"' || c == '`' || c == '[':
			endWord()
			seen()
			b.WriteRune(c)
			closer := c
			if c == '[' {
				closer = ']'
			}
			if err := s.copyQuoted(&b, closer); err != nil {
				return "", err
			}
			lastWord = ""
		case c == '-' && s.peek() == '-':
			endWord()
			b.WriteRune(c)
			for {
				c, _, err := s.r.ReadRune()
				if err != nil {
					break
				}
				b.WriteRune(c)
				if c == '\n' {
					s.line++
					break
				}
			}
		case c == '/' && s.peek() == '*':
			endWord()
			b.WriteRune(c)
			prev := rune(0)
			for {
				c, _, err := s.r.ReadRune()
				if err != nil {
					break
				}
				if c == '\n' {
					s.line++
				}
				b.WriteRune(c)
				if prev == '*' && c == '/' && b.Len() > 3 {
					break
				}
				prev = c
			}
		default:
			b.WriteRune(c)
			if unicode.IsLetter(c) || c == '_' || unicode.IsDigit(c) && word.Len() > 0 {
				word.WriteRune(c)
			} else {
				endWord()
				if !unicode.IsSpace(c) {
					lastWord = ""
				}
			}
			if !unicode.IsSpace(c) {
				seen()
			}
		}
	}
}

func (s *Scanner) peek() rune {
	c, _, err := s.r.ReadRune()
	if err != nil {
		return 0
	}
	s.r.UnreadRune()
	return c
}

// copyQuoted copies a quoted literal up to and including its closing quote
// (a doubled quote is an escaped quote).
func (s *Scanner) copyQuoted(b *strings.Builder, closer rune) error {
	for {
		c, _, err := s.r.ReadRune()
		if err == io.EOF {
			return nil // unterminated: the parser reports it
		}
		if err != nil {
			return err
		}
		if c == '\n' {
			s.line++
		}
		b.WriteRune(c)
		if c == closer {
			if closer != ']' && s.peek() == closer {
				c, _, _ = s.r.ReadRune()
				b.WriteRune(c)
				continue
			}
			return nil
		}
	}
}

// LeadingWords returns up to n leading words of a statement (identifiers
// upper-cased, quoted identifiers as written), skipping blanks and
// comments, without parsing the rest.
func LeadingWords(src string, n int) []string {
	var out []string
	i := 0
	for i < len(src) && len(out) < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case strings.HasPrefix(src[i:], "--"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return out
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return out
			}
			i += j + 4
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, strings.ToUpper(src[i:j]))
			i = j
		case c == '"' || c == '`' || c == '[':
			n := quotedLen(src[i:])
			toks, err := lex(src[i : i+n])
			if err != nil || len(toks) == 0 || toks[0].kind != tQuotedIdent {
				return out
			}
			out = append(out, toks[0].text)
			i += n
		default:
			return out
		}
	}
	return out
}

// quotedLen returns the length of the quoted identifier at the start of s.
func quotedLen(s string) int {
	end := s[0]
	if end == '[' {
		end = ']'
	}
	for j := 1; j < len(s); j++ {
		if s[j] == end {
			if end != ']' && j+1 < len(s) && s[j+1] == end {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}
