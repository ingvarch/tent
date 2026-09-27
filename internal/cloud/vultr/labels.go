// Package vultr is tent's Vultr provider.
package vultr

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/ingvarch/tent/internal/cloud"
)

// maxTags is the most tags tent puts on an instance: Vultr accepted 50 and rejected 100.
const maxTags = 50

// EncodeTags turns labels into instance tags, one "key=value" tag per label, sorted. Each key is cloud.LabelPrefix
// and a name without "=", each value is not empty, and both are lower case, because Vultr's tag filter ignores case.
// Any other label is an error, and so are more than 50 labels.
func EncodeTags(l cloud.Labels) ([]string, error) {
	if len(l) > maxTags {
		return nil, fmt.Errorf("%d labels: an instance takes at most %d tags", len(l), maxTags)
	}
	tags := make([]string, 0, len(l))
	for _, key := range slices.Sorted(maps.Keys(l)) {
		tag := key + "=" + l[key]
		if why := badLabel(key, l[key]); why != "" {
			return nil, fmt.Errorf("label %q: %s", tag, why)
		}
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags, nil
}

// DecodeTags turns instance tags back into labels. It ignores the tags outside cloud.LabelPrefix, which an operator
// may add, and fails on a malformed tag inside it. The prefix matches in any case, as Vultr's tag filter does, so
// "TENT/cluster=prod" is an error rather than a foreign tag.
func DecodeTags(tags []string) (cloud.Labels, error) {
	l := cloud.Labels{}
	for _, tag := range tags {
		if !hasLabelPrefix(tag) {
			continue
		}
		if why := addTag(l, tag); why != "" {
			return nil, fmt.Errorf("tag %q: %s", tag, why)
		}
	}
	return l, nil
}

// addTag adds a tag inside cloud.LabelPrefix to l, or returns why it cannot.
func addTag(l cloud.Labels, tag string) string {
	key, value, ok := strings.Cut(tag, "=")
	if !ok {
		return `no "="`
	}
	if why := badLabel(key, value); why != "" {
		return why
	}
	if _, dup := l[key]; dup {
		return "key given twice"
	}
	l[key] = value
	return ""
}

// badLabel returns why a label cannot be a tag, or "" when it can.
func badLabel(key, value string) string {
	switch {
	case !hasLabelPrefix(key):
		return fmt.Sprintf("no %q prefix", cloud.LabelPrefix)
	case len(key) == len(cloud.LabelPrefix):
		return fmt.Sprintf("no name after %q", cloud.LabelPrefix)
	case strings.Contains(key, "="):
		return `"=" in key`
	case value == "":
		return "empty value"
	case hasUpper(key) || hasUpper(value):
		return "upper case"
	}
	return ""
}

// hasLabelPrefix reports whether s starts with cloud.LabelPrefix in any case.
func hasLabelPrefix(s string) bool {
	n := len(cloud.LabelPrefix)
	return len(s) >= n && strings.EqualFold(s[:n], cloud.LabelPrefix)
}

// hasUpper reports whether s has a letter that lower-casing changes.
func hasUpper(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.ToLower(r) != r })
}

// Kinds of the objects that carry a Marker.
const (
	KindSSHKey       = "ssh-key"  // an SSH key, marked in its name
	KindVPC          = "vpc"      // a VPC, marked in its description
	KindFirewall     = "firewall" // a firewall group, marked in its description
	KindLoadBalancer = "lb"       // a load balancer, marked in its label
)

// markerPrefix starts every marker. A text without it is not tent's.
const markerPrefix = "tent:"

// Marker marks a Vultr object without tags as tent's. It is stored in the object's one free-text field as
// tent:cluster=<cluster>;kind=<kind>[;role=<role>][;name=<name>][;fp=<fingerprint>][;op=<op>].
type Marker struct {
	Cluster     string // the cluster that owns the object; required
	Kind        string // what the object is, such as KindVPC; required
	Role        string // the Nomad role a firewall group is for
	Name        string // tells objects of one kind apart, such as the api load balancer
	Fingerprint string // the start of an SSH key's fingerprint
	Op          string // the operation id of the call that created the object
}

// markerField is one field of a Marker and its key in the text.
type markerField struct {
	key   string
	value *string
}

// markerFields returns m's fields in the order of the text.
func markerFields(m *Marker) []markerField {
	return []markerField{
		{"cluster", &m.Cluster},
		{"kind", &m.Kind},
		{"role", &m.Role},
		{"name", &m.Name},
		{"fp", &m.Fingerprint},
		{"op", &m.Op},
	}
}

// String renders the marker as text and leaves out empty fields. It does not check the fields: the text parses back
// to the same marker only when Validate returns nil.
func (m Marker) String() string {
	var parts []string
	for _, f := range markerFields(&m) {
		if *f.value != "" {
			parts = append(parts, f.key+"="+*f.value)
		}
	}
	return markerPrefix + strings.Join(parts, ";")
}

// Validate checks the rules of every marker: Cluster and Kind are set, and no value has ";", "=" or upper case.
func (m Marker) Validate() error {
	if why := markerProblem(&m); why != "" {
		return fmt.Errorf("marker %q: %s", m.String(), why)
	}
	return nil
}

// markerProblem returns why m breaks a rule of Validate, or "" when it does not.
func markerProblem(m *Marker) string {
	switch {
	case m.Cluster == "":
		return "no cluster"
	case m.Kind == "":
		return "no kind"
	}
	for _, f := range markerFields(m) {
		switch v := *f.value; {
		case strings.Contains(v, ";"):
			return fmt.Sprintf(`";" in %s`, f.key)
		case strings.Contains(v, "="):
			return fmt.Sprintf(`"=" in %s`, f.key)
		case hasUpper(v):
			return "upper case in " + f.key
		}
	}
	return ""
}

// ParseMarker reads a marker from an object's free-text field. For a text that does not start with "tent:" it returns
// ok false and no error: the object is not tent's. For every other text ok is true, and an error reports a malformed
// marker: an empty field, a field without "=", an unknown key, a key given twice, an empty value, or a value that
// breaks a rule of Validate. The fields may come in any order.
func ParseMarker(s string) (Marker, bool, error) {
	text, ok := strings.CutPrefix(s, markerPrefix)
	if !ok {
		return Marker{}, false, nil
	}
	var m Marker
	why := parseMarkerFields(&m, text)
	if why == "" {
		why = markerProblem(&m)
	}
	if why != "" {
		return Marker{}, true, fmt.Errorf("marker %q: %s", s, why)
	}
	return m, true, nil
}

// parseMarkerFields sets m's fields from the text after "tent:", or returns why the text is malformed.
func parseMarkerFields(m *Marker, text string) string {
	if text == "" {
		return ""
	}
	fields := markerFields(m)
	for part := range strings.SplitSeq(text, ";") {
		key, value, ok := strings.Cut(part, "=")
		switch {
		case part == "":
			return "empty field"
		case !ok:
			return fmt.Sprintf(`no "=" in %q`, part)
		}
		i := slices.IndexFunc(fields, func(f markerField) bool { return f.key == key })
		switch {
		case i < 0:
			return fmt.Sprintf("unknown field %q", key)
		case *fields[i].value != "":
			return key + " given twice"
		case value == "":
			return "empty " + key
		}
		*fields[i].value = value
	}
	return ""
}
