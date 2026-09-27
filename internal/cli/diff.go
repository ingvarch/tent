package cli

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines a diff shows around a change.
const diffContext = 3

// diffLine is one line of a diff: op is ' ' for a line of both texts, '-' for a line of the first only and '+' for
// a line of the second only.
type diffLine struct {
	op   byte
	text string
}

// unifiedDiff returns the changes from a to b as a unified diff of "stored" and "edited", or "" when a and b have
// the same lines.
func unifiedDiff(a, b string) string {
	lines := diffLines(splitLines(a), splitLines(b))
	// aBefore[k] and bBefore[k] count the lines of a and of b before lines[k].
	aBefore := make([]int, len(lines)+1)
	bBefore := make([]int, len(lines)+1)
	for k, l := range lines {
		aBefore[k+1], bBefore[k+1] = aBefore[k], bBefore[k]
		if l.op != '+' {
			aBefore[k+1]++
		}
		if l.op != '-' {
			bBefore[k+1]++
		}
	}
	var out strings.Builder
	for _, h := range hunks(lines) {
		if out.Len() == 0 {
			out.WriteString("--- stored\n+++ edited\n")
		}
		lo, hi := h[0], h[1]
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aBefore[lo], aBefore[hi]-aBefore[lo]),
			hunkRange(bBefore[lo], bBefore[hi]-bBefore[lo]))
		for _, l := range lines[lo:hi] {
			out.WriteString(string(l.op) + l.text + "\n")
		}
	}
	return out.String()
}

// splitLines returns the lines of s without their line ends.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines returns the lines of a and b in order, each marked as a line of both or of one of them, with as many
// lines of both as there can be.
func diffLines(a, b []string) []diffLine {
	// common[i][j] is the length of the longest common subsequence of a[i:] and b[j:].
	common := make([][]int, len(a)+1)
	for i := range common {
		common[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				common[i][j] = common[i+1][j+1] + 1
			} else {
				common[i][j] = max(common[i+1][j], common[i][j+1])
			}
		}
	}
	var lines []diffLine
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			lines = append(lines, diffLine{' ', a[i]})
			i++
			j++
		case j == len(b) || i < len(a) && common[i+1][j] >= common[i][j+1]:
			lines = append(lines, diffLine{'-', a[i]})
			i++
		default:
			lines = append(lines, diffLine{'+', b[j]})
			j++
		}
	}
	return lines
}

// hunks returns the ranges [lo, hi) of lines that a unified diff shows: each change with up to diffContext unchanged
// lines around it, joined where they meet.
func hunks(lines []diffLine) [][2]int {
	var hs [][2]int
	for k, l := range lines {
		if l.op == ' ' {
			continue
		}
		lo, hi := max(k-diffContext, 0), min(k+diffContext+1, len(lines))
		if n := len(hs); n > 0 && lo <= hs[n-1][1] {
			hs[n-1][1] = hi
		} else {
			hs = append(hs, [2]int{lo, hi})
		}
	}
	return hs
}

// hunkRange writes the lines of one text that a hunk holds as the first line and the count, or with no lines as the
// line before them and 0.
func hunkRange(before, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", before)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}
