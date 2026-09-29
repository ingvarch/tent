package nodeconfig

import (
	"testing"

	"github.com/hashicorp/hcl"
)

// quoteCases are values that quote writes as HCL1 strings, and what it writes.
var quoteCases = []struct{ in, want string }{
	{"", `""`},
	{"platform", `"platform"`},
	{`say "hi"`, `"say \"hi\""`},
	{`C:\temp\`, `"C:\\temp\\"`},
	{`\"`, `"\\\""`},
	{`{{ GetPrivateInterfaces | attr "name" }}`, `"{{ GetPrivateInterfaces | attr \"name\" }}"`},
	{"%{ if true }x%{ endif }", `"%{ if true }x%{ endif }"`},
	{"$HOME costs $5 {and} $ {more}", `"$HOME costs $5 {and} $ {more}"`},
	{"Zürich 東京 Ω e\u0301 \u2028", "\"Zürich 東京 Ω e\u0301 \u2028\""},
}

func TestQuote(t *testing.T) {
	for _, tc := range quoteCases {
		got, err := quote(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("quote(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestQuoteRejects(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", "has the control character U+000A"},
		{"a\rb", "has the control character U+000D"},
		{"\t", "has the control character U+0009"},
		{"\x00", "has the control character U+0000"},
		{"\x7f", "has the control character U+007F"},
		{"next\u0085line", "has the control character U+0085"},
		{"\xff", "is not valid UTF-8"},
		{"\ue123", "has U+E123, which HCL1 reserves"},
		{"${node.unique.id}", "has ${, which HCL1 reads as the start of an interpolation"},
		{"$${escaped}", "has ${, which HCL1 reads as the start of an interpolation"},
		{"a${", "has ${, which HCL1 reads as the start of an interpolation"},
	} {
		if got, err := quote(tc.in); err == nil || err.Error() != tc.want {
			t.Errorf("quote(%q) = %q, %v; want the error %q", tc.in, got, err, tc.want)
		}
	}
}

// FuzzQuote checks that HCL1 reads every value that quote writes back as the value itself.
func FuzzQuote(f *testing.F) {
	for _, tc := range quoteCases {
		f.Add(tc.in)
	}
	for _, s := range []string{"a\nb", "${x}", "\ue123", "\xff", `"}`, `${"}`, "{{", "}}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := quote(s)
		if err != nil {
			return
		}
		var got struct {
			V string `hcl:"v"`
		}
		if err := hcl.Decode(&got, "v = "+q+"\n"); err != nil {
			t.Fatalf("HCL1 cannot read quote(%q) = %s: %v", s, q, err)
		}
		if got.V != s {
			t.Fatalf("HCL1 reads quote(%q) = %s as %q", s, q, got.V)
		}
	})
}
