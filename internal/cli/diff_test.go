package cli

import (
	"strconv"
	"strings"
	"testing"
)

// numbered returns the lines 1 to n, each its number, with the lines of changes in place of theirs.
func numbered(n int, changes map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		line, ok := changes[i]
		if !ok {
			line = strconv.Itoa(i)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func TestUnifiedDiff(t *testing.T) {
	for _, tc := range []struct {
		name, a, b, want string
	}{
		{"no change", "a\nb\n", "a\nb\n", ""},
		{"both empty", "", "", ""},
		{"a change in the middle", "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "1\n2\n3\n4\nfive\n6\n7\n8\n9\n", `--- stored
+++ edited
@@ -2,7 +2,7 @@
 2
 3
 4
-5
+five
 6
 7
 8
`},
		{"an addition at the end", "a\nb\n", "a\nb\nc\n", `--- stored
+++ edited
@@ -1,2 +1,3 @@
 a
 b
+c
`},
		{"a removal at the start", "a\nb\nc\n", "b\nc\n", `--- stored
+++ edited
@@ -1,3 +1,2 @@
-a
 b
 c
`},
		{"removals before additions", "a\nb\nc\n", "A\nB\nc\n", `--- stored
+++ edited
@@ -1,3 +1,3 @@
-a
-b
+A
+B
 c
`},
		{"all added", "", "a\nb\n", `--- stored
+++ edited
@@ -0,0 +1,2 @@
+a
+b
`},
		{"all removed", "a\n", "", `--- stored
+++ edited
@@ -1,1 +0,0 @@
-a
`},
		{"far apart changes in two hunks", numbered(20, nil), numbered(20, map[int]string{2: "two", 18: "eighteen"}),
			`--- stored
+++ edited
@@ -1,5 +1,5 @@
 1
-2
+two
 3
 4
 5
@@ -15,6 +15,6 @@
 15
 16
 17
-18
+eighteen
 19
 20
`},
		{"changes six lines apart in one hunk", numbered(20, nil),
			numbered(20, map[int]string{5: "five", 12: "twelve"}), `--- stored
+++ edited
@@ -2,14 +2,14 @@
 2
 3
 4
-5
+five
 6
 7
 8
 9
 10
 11
-12
+twelve
 13
 14
 15
`},
		{"changes seven lines apart in two hunks", numbered(20, nil),
			numbered(20, map[int]string{5: "five", 13: "thirteen"}), `--- stored
+++ edited
@@ -2,7 +2,7 @@
 2
 3
 4
-5
+five
 6
 7
 8
@@ -10,7 +10,7 @@
 10
 11
 12
-13
+thirteen
 14
 15
 16
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unifiedDiff(tc.a, tc.b); got != tc.want {
				t.Errorf("unifiedDiff:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
