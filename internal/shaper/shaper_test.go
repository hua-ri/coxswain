package shaper

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShapeSmallUnchanged(t *testing.T) {
	a := NewArtifacts(t.TempDir())
	got, err := a.Shape("a\nb\n", Default)
	if err != nil || got.Truncated || got.Text != "a\nb\n" || got.TotalLines != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestShapeLargeSavesArtifact(t *testing.T) {
	a := NewArtifacts(t.TempDir())
	var sb strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	got, err := a.Shape(sb.String(), Limits{MaxBytes: 1 << 20, MaxLines: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.ArtifactID == "" || got.TotalLines != 1000 {
		t.Fatalf("got %+v", got)
	}
	if !strings.HasPrefix(got.Text, "line 1\n") || !strings.HasSuffix(got.Text, "line 1000") {
		t.Fatalf("head/tail wrong: %q", got.Text)
	}
	if !strings.Contains(got.Text, "990 lines omitted") {
		t.Fatalf("missing omission note: %q", got.Text)
	}

	part, err := a.Read(got.ArtifactID, `^line 50\d$`, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(part, "500: line 500") || strings.Count(part, "\n") != 9 {
		t.Fatalf("grep read: %q", part)
	}
	rng, _ := a.Read(got.ArtifactID, "", 3, 4)
	if rng != "3: line 3\n4: line 4" {
		t.Fatalf("range read: %q", rng)
	}
}

func TestShapeRespectsBytes(t *testing.T) {
	a := NewArtifacts(t.TempDir())
	got, _ := a.Shape(strings.Repeat("x", 100)+"\n"+strings.Repeat("y", 100000), Limits{MaxBytes: 1000, MaxLines: 200})
	if !got.Truncated || len(got.Text) > 1200 {
		t.Fatalf("len %d, truncated %v", len(got.Text), got.Truncated)
	}
}

func TestReadRejectsTraversal(t *testing.T) {
	if _, err := NewArtifacts(t.TempDir()).Read("../x", "", 0, 0); err == nil {
		t.Fatal("expected error")
	}
}

// TestClipKeepsRunes: a long line is never cut inside a UTF-8 sequence.
func TestClipKeepsRunes(t *testing.T) {
	line := strings.Repeat("中文", 10) // 3 bytes per rune
	for _, max := range []int{1, 2, 3, 4, 5, 7, 11, len(line)} {
		head := clip([]string{line}, max)[0]
		tail := clipTail([]string{line}, max)[0]
		if !utf8.ValidString(head) || !utf8.ValidString(tail) {
			t.Fatalf("max %d: head %q tail %q", max, head, tail)
		}
		if len(strings.TrimSuffix(head, "…")) > max || len(strings.TrimPrefix(tail, "…")) > max {
			t.Fatalf("max %d: too long: %q %q", max, head, tail)
		}
	}
}

func TestClip(t *testing.T) {
	if got, cut := Clip("a\nb\n", Limits{MaxBytes: 100, MaxLines: 20}); got != "a\nb\n" || cut {
		t.Fatalf("%q %v", got, cut)
	}
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	got, cut := Clip(sb.String(), Limits{MaxBytes: 1 << 20, MaxLines: 20})
	if !cut || !strings.HasPrefix(got, "line 1\n") || !strings.HasSuffix(got, "line 100") || !strings.Contains(got, "80 lines omitted") ||
		strings.Count(got, "\n") != 20 {
		t.Fatalf("%q", got)
	}
}

func TestSave(t *testing.T) {
	a := NewArtifacts(t.TempDir())
	id, err := a.Save("x\ny\n")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Read(id, "", 2, 2); err != nil || got != "2: y" {
		t.Fatalf("%q %v", got, err)
	}
}
