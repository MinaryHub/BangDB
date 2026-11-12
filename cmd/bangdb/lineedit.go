package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// lineEditor reads lines from a terminal with editing keys, a history
// (arrow up/down, kept in a file) and Tab completion. The terminal is in
// raw mode only while a line is being read.
//
//	left/right, Ctrl-B/F     move by a character      Ctrl-left/right, Alt-B/F  by a word
//	Home/End, Ctrl-A/E       start / end of the line
//	Backspace, Delete        delete a character       Ctrl-W  the word before the cursor
//	Ctrl-U / Ctrl-K          delete to the start / end of the line
//	up/down, Ctrl-P/N        previous / next line of the history
//	Tab                      complete the word; again to list the candidates
//	Ctrl-L                   clear the screen
//	Ctrl-C                   abandon the line         Ctrl-D  end of input (on an empty line)
type lineEditor struct {
	in  *bufio.Reader
	out io.Writer
	// raw puts the terminal into raw mode and returns the function that
	// restores it; width returns the number of columns of the terminal.
	raw   func() (restore func(), ok bool)
	width func() int
	// complete returns the candidates for the word that ends at rune
	// position pos of line, and the position where that word starts.
	complete func(line []rune, pos int) (start int, candidates []string)

	history  []string
	histFile string // "" = not kept
}

const historyMax = 1000

// errAbandoned is returned by readLine when the line was given up (Ctrl-C).
var errAbandoned = errors.New("line abandoned")

func newLineEditor(in io.Reader, out io.Writer, histFile string) *lineEditor {
	ed := &lineEditor{in: bufio.NewReader(in), out: out, raw: rawMode, width: termWidth, histFile: histFile}
	if histFile != "" {
		if data, err := os.ReadFile(histFile); err == nil {
			for _, l := range strings.Split(string(data), "\n") {
				if l = strings.TrimRight(l, "\r"); l != "" {
					ed.history = append(ed.history, l)
				}
			}
			if len(ed.history) > historyMax {
				ed.history = ed.history[len(ed.history)-historyMax:]
				// Rewrite the file so that it does not grow without bound.
				os.WriteFile(histFile, []byte(strings.Join(ed.history, "\n")+"\n"), 0o600)
			}
		}
	}
	return ed
}

// remember adds an entered line to the history.
func (ed *lineEditor) remember(line string) {
	if strings.TrimSpace(line) == "" || len(ed.history) > 0 && ed.history[len(ed.history)-1] == line {
		return
	}
	ed.history = append(ed.history, line)
	if ed.histFile == "" {
		return
	}
	if f, err := os.OpenFile(ed.histFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.WriteString(line + "\n")
		f.Close()
	}
}

// Keys that are not characters.
const (
	keyUp = -(iota + 1)
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyDelete
	keyWordLeft
	keyWordRight
	keyDeleteWord
	keyNone
)

// readKey reads a character or decodes the escape sequence of a key.
func (ed *lineEditor) readKey() (rune, error) {
	r, _, err := ed.in.ReadRune()
	if err != nil || r != 0x1b {
		return r, err
	}
	if ed.in.Buffered() == 0 {
		return keyNone, nil // a lone Escape
	}
	b, _ := ed.in.ReadByte()
	switch b {
	case '[':
		var seq []byte
		for {
			c, err := ed.in.ReadByte()
			if err != nil {
				return 0, err
			}
			seq = append(seq, c)
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
		word := strings.Contains(string(seq), ";5") || strings.Contains(string(seq), ";3") // with Ctrl or Alt
		switch final, params := seq[len(seq)-1], string(seq[:len(seq)-1]); final {
		case 'A':
			return keyUp, nil
		case 'B':
			return keyDown, nil
		case 'C':
			if word {
				return keyWordRight, nil
			}
			return keyRight, nil
		case 'D':
			if word {
				return keyWordLeft, nil
			}
			return keyLeft, nil
		case 'H':
			return keyHome, nil
		case 'F':
			return keyEnd, nil
		case '~':
			switch params {
			case "1", "7":
				return keyHome, nil
			case "4", "8":
				return keyEnd, nil
			case "3":
				return keyDelete, nil
			}
		}
	case 'O':
		switch c, _ := ed.in.ReadByte(); c {
		case 'A':
			return keyUp, nil
		case 'B':
			return keyDown, nil
		case 'C':
			return keyRight, nil
		case 'D':
			return keyLeft, nil
		case 'H':
			return keyHome, nil
		case 'F':
			return keyEnd, nil
		}
	case 'b':
		return keyWordLeft, nil
	case 'f':
		return keyWordRight, nil
	case 0x7f, 0x08:
		return keyDeleteWord, nil
	}
	return keyNone, nil
}

// readLine shows the prompt and returns the line the user enters, io.EOF at
// the end of the input, or errAbandoned.
func (ed *lineEditor) readLine(prompt string) (string, error) {
	restore, ok := ed.raw()
	if !ok {
		return "", errors.New("the terminal cannot be put into raw mode")
	}
	defer restore()

	var buf []rune
	pos := 0
	hist := len(ed.history) // the history entry shown; len = the new line
	var pending []rune      // the new line, while an entry is shown
	tabbed := false         // the last key was a Tab that completed nothing
	refresh := func() { ed.refresh(prompt, buf, pos) }
	show := func(i int) {
		if hist == len(ed.history) {
			pending = buf
		}
		hist = i
		if i == len(ed.history) {
			buf = pending
		} else {
			buf = []rune(ed.history[i])
		}
		pos = len(buf)
	}
	wordLeft := func() int {
		i := pos
		for i > 0 && !isWordRune(buf[i-1]) {
			i--
		}
		for i > 0 && isWordRune(buf[i-1]) {
			i--
		}
		return i
	}
	refresh()
	for {
		k, err := ed.readKey()
		if err != nil {
			fmt.Fprint(ed.out, "\r\n")
			if len(buf) > 0 && err == io.EOF {
				return string(buf), nil // a last line without a newline
			}
			return "", err
		}
		wasTab := tabbed
		tabbed = false
		switch k {
		case '\r', '\n':
			if k == '\r' && ed.in.Buffered() > 0 {
				if b, _ := ed.in.Peek(1); b[0] == '\n' {
					ed.in.ReadByte() // pasted CR LF
				}
			}
			pos = len(buf)
			refresh()
			fmt.Fprint(ed.out, "\r\n")
			line := string(buf)
			ed.remember(line)
			return line, nil
		case 3: // Ctrl-C
			fmt.Fprint(ed.out, "^C\r\n")
			return "", errAbandoned
		case 4: // Ctrl-D
			if len(buf) == 0 {
				fmt.Fprint(ed.out, "\r\n")
				return "", io.EOF
			}
			if pos < len(buf) {
				buf = append(buf[:pos], buf[pos+1:]...)
			}
		case keyDelete:
			if pos < len(buf) {
				buf = append(buf[:pos], buf[pos+1:]...)
			}
		case 0x7f, 8: // Backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case keyLeft, 2:
			pos = max(pos-1, 0)
		case keyRight, 6:
			pos = min(pos+1, len(buf))
		case keyHome, 1:
			pos = 0
		case keyEnd, 5:
			pos = len(buf)
		case keyWordLeft:
			pos = wordLeft()
		case keyWordRight:
			for pos < len(buf) && !isWordRune(buf[pos]) {
				pos++
			}
			for pos < len(buf) && isWordRune(buf[pos]) {
				pos++
			}
		case 23, keyDeleteWord: // Ctrl-W
			i := wordLeft()
			buf = append(buf[:i], buf[pos:]...)
			pos = i
		case 21: // Ctrl-U
			buf = append([]rune(nil), buf[pos:]...)
			pos = 0
		case 11: // Ctrl-K
			buf = buf[:pos]
		case 12: // Ctrl-L
			fmt.Fprint(ed.out, "\x1b[H\x1b[2J")
		case keyUp, 16:
			if hist > 0 {
				show(hist - 1)
			}
		case keyDown, 14:
			if hist < len(ed.history) {
				show(hist + 1)
			}
		case '\t':
			if ed.in.Buffered() > 0 || ed.complete == nil { // pasted text: a tab is white space
				buf = append(buf[:pos], append([]rune{' '}, buf[pos:]...)...)
				pos++
				break
			}
			start, cands := ed.complete(buf, pos)
			if len(cands) == 0 {
				break
			}
			word := []rune(commonPrefix(cands))
			if len(cands) == 1 {
				word = append(word, ' ')
			}
			if len(word) > pos-start || len(cands) == 1 {
				buf = append(buf[:start], append(word, buf[pos:]...)...)
				pos = start + len(word)
				break
			}
			if tabbed = true; wasTab { // the second Tab lists the candidates
				fmt.Fprint(ed.out, "\r\n"+columns(cands, ed.width())+"\r")
			}
		default:
			if k >= 32 && k != keyNone {
				buf = append(buf[:pos], append([]rune{k}, buf[pos:]...)...)
				pos++
			}
		}
		refresh()
	}
}

// refresh redraws the line and puts the cursor at pos. A line wider than
// the terminal is shown as the part around the cursor.
func (ed *lineEditor) refresh(prompt string, buf []rune, pos int) {
	avail := ed.width() - displayWidth(prompt) - 1
	if avail < 8 {
		avail = 8
	}
	start := 0
	for runesWidth(buf[start:pos]) > avail {
		start++
	}
	end, w := start, 0
	for end < len(buf) && w+runeWidth(buf[end]) <= avail {
		w += runeWidth(buf[end])
		end++
	}
	var sb strings.Builder
	sb.WriteString("\r" + prompt + string(buf[start:end]) + "\x1b[K\r")
	if n := displayWidth(prompt) + runesWidth(buf[start:pos]); n > 0 {
		fmt.Fprintf(&sb, "\x1b[%dC", n)
	}
	io.WriteString(ed.out, sb.String())
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// commonPrefix returns the longest prefix the words share, ignoring case
// (spelled as in the first word).
func commonPrefix(words []string) string {
	p := []rune(words[0])
	for _, w := range words[1:] {
		r := []rune(w)
		n := 0
		for n < len(p) && n < len(r) && unicode.ToLower(p[n]) == unicode.ToLower(r[n]) {
			n++
		}
		p = p[:n]
	}
	return string(p)
}

// columns lays words out in columns for a terminal of the given width
// (lines end with CR LF, as the terminal is in raw mode).
func columns(words []string, width int) string {
	const most = 100
	more := 0
	if len(words) > most {
		more, words = len(words)-most, words[:most]
	}
	w := 0
	for _, s := range words {
		w = max(w, displayWidth(s))
	}
	w += 2
	per := max(width/w, 1)
	var sb strings.Builder
	for i, s := range words {
		sb.WriteString(s)
		if (i+1)%per == 0 || i == len(words)-1 {
			sb.WriteString("\r\n")
		} else {
			sb.WriteString(strings.Repeat(" ", w-displayWidth(s)))
		}
	}
	if more > 0 {
		fmt.Fprintf(&sb, "... and %d more\r\n", more)
	}
	return sb.String()
}

// runeWidth is the number of terminal columns a character takes: 2 for
// East Asian wide characters (Hangul, CJK, full-width forms, emoji), 0 for
// combining marks.
func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		if r >= 0x300 && r < 0x370 {
			return 0
		}
		return 1
	case r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,              // CJK, Kana, Bopomofo, Yi
		r >= 0xac00 && r <= 0xd7a3,                             // Hangul syllables
		r >= 0xf900 && r <= 0xfaff,                             // CJK compatibility ideographs
		r >= 0xfe30 && r <= 0xfe6f,                             // CJK compatibility forms
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6, // full-width forms
		r >= 0x1f300 && r <= 0x1faff, // emoji
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	if unicode.Is(unicode.Mn, r) {
		return 0
	}
	return 1
}

func runesWidth(rs []rune) int {
	w := 0
	for _, r := range rs {
		w += runeWidth(r)
	}
	return w
}

// displayWidth is the number of terminal columns s takes.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}
