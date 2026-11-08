package term

import "testing"

func TestPlain(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "hello\nworld\n", "hello\nworld\n"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"colors", "\x1b[1;31mred\x1b[0m ok", "red ok"},
		{"carriage return overwrites", "progress 10%\rprogress 100%\n", "progress 100%\n"},
		{"shorter overwrite keeps the rest", "abcdef\rXY\n", "XYcdef\n"},
		{"erase to end of line", "abcdef\r\x1b[Kxy\n", "xy\n"},
		{"erase whole line", "abc\x1b[2K\rz", "z"},
		{"backspace", "abd\bc\n", "abc\n"},
		{"osc title with bel", "\x1b]0;user@host\x07$ ", "$ "},
		{"osc title with st", "\x1b]2;t\x1b\\$ ", "$ "},
		{"charset", "\x1b(Bok", "ok"},
		{"private mode", "\x1b[?2004hx\x1b[?2004l", "x"},
		{"other controls dropped", "a\x00\x07b", "ab"},
		{"utf8", "héllo 世界\n", "héllo 世界\n"},
		{"tab kept", "a\tb", "a\tb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Plain([]byte(c.in)); got != c.want {
				t.Fatalf("Plain(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLastLines(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		n       int
		partial bool
		want    string
	}{
		{"all", "a\nb\n$ ", 10, false, "a\nb\n$ "},
		{"last two", "a\nb\nc\n", 2, false, "b\nc"},
		{"trailing blank lines dropped", "a\n\n\n", 5, false, "a"},
		{"partial first line dropped", "tail of x\nb\nc", 10, true, "b\nc"},
		{"empty", "", 5, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lastLines(c.in, c.n, c.partial); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
