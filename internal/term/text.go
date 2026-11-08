package term

import (
	"strings"
	"unicode/utf8"
)

// Plain renders terminal output as plain text: escape sequences are
// removed, \r returns to the start of the line (later text overwrites
// it), backspace moves left, erase-in-line (ESC [ K) is honoured and other
// control characters are dropped. It is not a terminal emulator: output
// of full-screen programs (top, vim) comes out garbled.
func Plain(b []byte) string {
	var out strings.Builder
	var line []rune
	col := 0
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\n':
			out.WriteString(string(line))
			out.WriteByte('\n')
			line, col = line[:0], 0
			i++
		case c == '\r':
			col = 0
			i++
		case c == '\b':
			col = max(col-1, 0)
			i++
		case c == 0x1b:
			var final byte
			var param string
			i, final, param = escape(b, i)
			if final == 'K' {
				switch param {
				case "", "0":
					line = line[:min(col, len(line))]
				case "1":
					for j := 0; j < min(col+1, len(line)); j++ {
						line[j] = ' '
					}
				case "2":
					line = line[:0]
				}
			}
		case c == '\t' || c >= 0x20 && c != 0x7f:
			r, n := utf8.DecodeRune(b[i:])
			i += n
			if r == utf8.RuneError && n <= 1 {
				continue // a cut or invalid sequence
			}
			for len(line) < col {
				line = append(line, ' ')
			}
			if col < len(line) {
				line[col] = r
			} else {
				line = append(line, r)
			}
			col++
		default:
			i++
		}
	}
	out.WriteString(string(line))
	return out.String()
}

// escape skips the escape sequence starting at b[i] (ESC) and returns
// the index after it; for a CSI sequence it also returns the final byte
// and the parameters.
func escape(b []byte, i int) (next int, final byte, param string) {
	i++
	if i >= len(b) {
		return i, 0, ""
	}
	switch b[i] {
	case '[': // CSI: parameters, intermediates, one final byte
		start := i + 1
		for i = start; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1, b[i], string(b[start:i])
			}
		}
		return i, 0, ""
	case ']', 'P', 'X', '^', '_': // strings ended by BEL or ESC \
		for i++; i < len(b); i++ {
			if b[i] == 0x07 {
				return i + 1, 0, ""
			}
			if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
				return i + 2, 0, ""
			}
		}
		return i, 0, ""
	case '(', ')', '*', '+': // character set: one more byte
		return min(i+2, len(b)), 0, ""
	default:
		return i + 1, 0, ""
	}
}

// lastLines returns the last n lines of s, without trailing blank lines.
// If partial, the first line is cut (the start of the output was
// dropped) and is left out.
func lastLines(s string, n int, partial bool) string {
	if partial {
		_, s, _ = strings.Cut(s, "\n")
	}
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines[max(len(lines)-n, 0):], "\n")
}
