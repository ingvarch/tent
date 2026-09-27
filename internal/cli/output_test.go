package cli

import (
	"bytes"
	"io"
	"testing"
)

func TestPrintObjectJSONKeepsHTMLCharacters(t *testing.T) {
	var out bytes.Buffer
	if err := printObject(&out, outputJSON, map[string]string{"name": "<a & b>"}, nil); err != nil {
		t.Fatalf("printObject: %v", err)
	}
	want := "{\n  \"name\": \"<a & b>\"\n}\n"
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
}

func TestPrintObjectRejectsAnUnknownFormat(t *testing.T) {
	// cobra skips the root's -o check when a subcommand has its own PersistentPreRunE.
	var out bytes.Buffer
	err := printObject(&out, "xml", struct{}{}, func(w io.Writer) error {
		_, err := io.WriteString(w, "a table\n")
		return err
	})
	want := `invalid output format "xml": want table, yaml or json`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q, want nothing", out.String())
	}
}
