package english_test

import (
	"testing"

	"github.com/ingvarch/tent/internal/english"
)

func TestAnd(t *testing.T) {
	for _, tc := range []struct {
		items []string
		want  string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a and b"},
		{[]string{"a", "b", "c"}, "a, b and c"},
	} {
		if got := english.And(tc.items); got != tc.want {
			t.Errorf("And(%q) = %q, want %q", tc.items, got, tc.want)
		}
	}
}
