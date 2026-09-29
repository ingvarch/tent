package nodeconfig

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// hclReserved is a character that the HCL1 scanner refuses anywhere in a file.
const hclReserved = '\uE123'

// quote returns s as an HCL1 string: in double quotes, with each " and \ escaped by a backslash, and everything else,
// Unicode included, as it is. It refuses what HCL1 cannot read back as s:
//   - text that is not valid UTF-8;
//   - control characters, such as a line end or a tab, and U+E123, which the HCL1 scanner refuses;
//   - "${", which HCL1 reads as the start of an interpolation and has no escape for.
//
// "{{", "%{" and "$" alone stay as they are: HCL1 reads them as text. Its errors never show s, which may be a secret.
func quote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("is not valid UTF-8")
	}
	if i := strings.IndexFunc(s, unicode.IsControl); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return "", fmt.Errorf("has the control character %U", r)
	}
	if strings.ContainsRune(s, hclReserved) {
		return "", fmt.Errorf("has %U, which HCL1 reserves", hclReserved)
	}
	if strings.Contains(s, "${") {
		return "", errors.New("has ${, which HCL1 reads as the start of an interpolation")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}
