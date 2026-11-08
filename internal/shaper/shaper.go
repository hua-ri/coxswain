// Package shaper keeps tool output small enough for an LLM context.
// Oversized output is saved as an artifact and replaced by its head and
// tail plus an artifact id that can be read on demand.
package shaper

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Limits struct {
	MaxBytes int
	MaxLines int
}

var Default = Limits{MaxBytes: 8 * 1024, MaxLines: 200}

type Shaped struct {
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated,omitempty"`
	TotalLines int    `json:"total_lines"`
	TotalBytes int    `json:"total_bytes"`
	ArtifactID string `json:"artifact_id,omitempty"`
}

type Artifacts struct{ dir string }

func NewArtifacts(dir string) *Artifacts { return &Artifacts{dir: dir} }

var idPattern = regexp.MustCompile(`^art_[0-9a-f]{12}$`)

// Shape returns s unchanged if it fits the limits; otherwise it stores s
// as an artifact and returns its head and tail.
func (a *Artifacts) Shape(s string, lim Limits) (Shaped, error) {
	lines := splitLines(s)
	out := Shaped{Text: s, TotalLines: len(lines), TotalBytes: len(s)}
	if len(s) <= lim.MaxBytes && len(lines) <= lim.MaxLines {
		return out, nil
	}
	id, err := a.Save(s)
	if err != nil {
		return Shaped{}, err
	}
	out.Text = headTail(lines, lim, fmt.Sprintf("full output is artifact %s (read with cox_artifact_read)", id))
	out.Truncated = true
	out.ArtifactID = id
	return out, nil
}

// Clip returns s unchanged if it fits the limits; otherwise its head and
// tail with a note of how many lines were left out (nothing is saved).
func Clip(s string, lim Limits) (string, bool) {
	lines := splitLines(s)
	if len(s) <= lim.MaxBytes && len(lines) <= lim.MaxLines {
		return s, false
	}
	return headTail(lines, lim, ""), true
}

// headTail keeps the first and last lines within lim around a note.
func headTail(lines []string, lim Limits, note string) string {
	keep := lim.MaxLines / 2
	headN := min(keep, len(lines))
	head := clip(lines[:headN], lim.MaxBytes/2)
	tail := clipTail(lines[max(len(lines)-keep, headN):], lim.MaxBytes/2)
	omitted := len(lines) - len(head) - len(tail)
	if note != "" {
		note = "; " + note
	}
	return strings.Join(head, "\n") + fmt.Sprintf("\n… %d lines omitted%s …\n", omitted, note) + strings.Join(tail, "\n")
}

// Read returns lines [start, end] (1-based, inclusive; 0 means open) of an
// artifact, optionally only those matching grep. At most 500 lines.
func (a *Artifacts) Read(id, grep string, start, end int) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid artifact id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(a.dir, id+".txt"))
	if err != nil {
		return "", fmt.Errorf("artifact %s: %w", id, err)
	}
	var re *regexp.Regexp
	if grep != "" {
		if re, err = regexp.Compile(grep); err != nil {
			return "", fmt.Errorf("invalid grep pattern: %w", err)
		}
	}
	lines := splitLines(string(b))
	if start < 1 {
		start = 1
	}
	if end < 1 || end > len(lines) {
		end = len(lines)
	}
	var out []string
	for i := start; i <= end && len(out) < 500; i++ {
		if re == nil || re.MatchString(lines[i-1]) {
			out = append(out, fmt.Sprintf("%d: %s", i, lines[i-1]))
		}
	}
	return strings.Join(out, "\n"), nil
}

// Save stores s as a new artifact and returns its id.
func (a *Artifacts) Save(s string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := "art_" + hex.EncodeToString(b[:])
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(filepath.Join(a.dir, id+".txt"), []byte(s), 0o600)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// clip keeps leading lines until maxBytes is reached. A first line that
// alone exceeds maxBytes is cut at a rune boundary.
func clip(lines []string, maxBytes int) []string {
	n := 0
	for i, l := range lines {
		n += len(l) + 1
		if n > maxBytes {
			if i == 0 {
				cut := maxBytes
				for cut > 0 && cut < len(l) && !utf8.RuneStart(l[cut]) {
					cut--
				}
				return []string{l[:cut] + "…"}
			}
			return lines[:i]
		}
	}
	return lines
}

// clipTail keeps trailing lines until maxBytes is reached. A last line
// that alone exceeds maxBytes is cut at a rune boundary.
func clipTail(lines []string, maxBytes int) []string {
	n := 0
	for i := len(lines) - 1; i >= 0; i-- {
		n += len(lines[i]) + 1
		if n > maxBytes {
			if i == len(lines)-1 {
				l := lines[i]
				start := len(l) - maxBytes
				for start < len(l) && !utf8.RuneStart(l[start]) {
					start++
				}
				return []string{"…" + l[start:]}
			}
			return lines[i+1:]
		}
	}
	return lines
}
