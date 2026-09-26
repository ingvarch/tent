package statestore

import (
	"errors"
	"fmt"
	"strings"
)

// validPath checks an object path against the rules in the package documentation.
func validPath(p string) error {
	if why := badPath(p); why != "" {
		return errors.New("invalid path: " + why)
	}
	return nil
}

// validPrefix checks a List prefix: empty, or whole segments followed by a slash or by an unfinished segment.
func validPrefix(prefix string) error {
	why := ""
	last := prefix
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		why, last = badSegments(prefix[:i]), prefix[i+1:]
	}
	if why == "" && last != "" {
		why = badChars(last)
	}
	if why != "" {
		return errors.New("invalid prefix: " + why)
	}
	return nil
}

// badPath returns why p is not a valid object path, or "" when it is.
func badPath(p string) string {
	if p == "" {
		return "empty"
	}
	return badSegments(p)
}

// badSegments returns why one of the slash-separated segments of p is invalid, or "" when none is.
func badSegments(p string) string {
	for seg := range strings.SplitSeq(p, "/") {
		if why := badSegment(seg); why != "" {
			return why
		}
	}
	return ""
}

// badSegment returns why seg is not a valid path segment, or "" when it is.
func badSegment(seg string) string {
	if why := badChars(seg); why != "" {
		return why
	}
	switch {
	case strings.HasSuffix(seg, "."):
		return fmt.Sprintf("segment %q ends with '.'", seg)
	case windowsDevice(seg):
		return fmt.Sprintf("segment %q is a Windows device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9)", seg)
	}
	return ""
}

// badChars checks the characters of a segment, which may be the unfinished last segment of a List prefix.
func badChars(seg string) string {
	switch {
	case seg == "":
		return "an empty segment: separate segments with single slashes, none at the start or end"
	case seg == "." || seg == "..":
		return fmt.Sprintf("segment %q: paths have no . or .. segments", seg)
	case seg[0] == '.':
		return fmt.Sprintf("segment %q starts with '.': such names are reserved for the store", seg)
	}
	for i := range len(seg) {
		if !nameChar(seg[i], i == 0) {
			return fmt.Sprintf("segment %q: use only ASCII letters, digits, '_', '-' and '.', "+
				"and start with a letter, digit or '_'", seg)
		}
	}
	return ""
}

// nameChar reports whether c may appear in a segment, at its start when first is set.
func nameChar(c byte, first bool) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '_':
		return true
	case c == '.' || c == '-':
		return !first
	}
	return false
}

// windowsDevice reports whether Windows opens a device for seg: CON, PRN, AUX, NUL, COM1-9 or LPT1-9, in any case,
// also with an extension. api/v1alpha1 rejects the same names for clusters and node groups; change both together.
func windowsDevice(seg string) bool {
	base, _, _ := strings.Cut(seg, ".")
	base = strings.ToUpper(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) &&
		'1' <= base[3] && base[3] <= '9'
}
