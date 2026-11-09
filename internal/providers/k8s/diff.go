package k8s

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines surround a change.
const diffContext = 3

// maxDiffCells bounds the work of a diff; larger inputs show as replaced.
const maxDiffCells = 4_000_000

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// diffLines returns a unified diff of a and b (split into lines), or ""
// if they are equal.
func diffLines(oldName, newName, a, b string) string {
	if a == b {
		return ""
	}
	ops := diffOps(splitText(a), splitText(b))
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	// Positions (0-based) of each op in a and b.
	ai, bi := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, op := range ops {
		ai[i+1], bi[i+1] = ai[i], bi[i]
		if op.kind != '+' {
			ai[i+1]++
		}
		if op.kind != '-' {
			bi[i+1]++
		}
	}
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-diffContext, 0)
		end := i // last changed op in the hunk
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				if j-end > 2*diffContext {
					break
				}
				end = j
			}
		}
		stop := min(end+diffContext+1, len(ops))
		oldN, newN := ai[stop]-ai[start], bi[stop]-bi[start]
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(ai[start], oldN), hunkRange(bi[start], newN))
		for _, op := range ops[start:stop] {
			out.WriteByte(op.kind)
			out.WriteString(op.line)
			out.WriteByte('\n')
		}
		i = stop
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func splitText(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffOps is an edit script from a to b by longest common subsequence;
// deletions come before insertions.
func diffOps(a, b []string) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > maxDiffCells {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the LCS length of a[i:] and b[j:].
	w := len(b) + 1
	lcs := make([]int, (len(a)+1)*w)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
