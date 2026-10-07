package e2e

import (
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ingvarch/tent/test/e2e/janitor"
)

func TestNewRunIDMapsBytesToTheAlphabet(t *testing.T) {
	// 0 is a, 25 is z, 26 is 0, 35 is 9, and 36 wraps to a.
	got, err := newRunID(strings.NewReader("\x00\x19\x1a\x23\x24\x25"))
	if err != nil {
		t.Fatalf("newRunID: %v", err)
	}
	if want := "az09ab"; got != want {
		t.Errorf("newRunID = %q, want %q", got, want)
	}
}

func TestNewRunIDFromTheSystemRandomness(t *testing.T) {
	id, err := newRunID(rand.Reader)
	if err != nil {
		t.Fatalf("newRunID: %v", err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}$`).MatchString(id) {
		t.Errorf("run id %q is not 6 characters of [a-z0-9]", id)
	}
}

func TestNewRunIDReportsAShortRead(t *testing.T) {
	boom := errors.New("no randomness")
	if _, err := newRunID(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("error = %v, want one wrapping %v", err, boom)
	}
	if _, err := newRunID(strings.NewReader("abc")); err == nil {
		t.Error("a reader of 3 bytes gave a run id")
	}
}

func TestClusterNameFollowsTentsRuleAndTheJanitorsPrefix(t *testing.T) {
	rule := regexp.MustCompile(`^[a-z][a-z0-9-]{0,18}[a-z0-9]$`)
	for image, want := range map[string]string{
		"ubuntu-24.04": "e2e-ab12cd-2404",
		"ubuntu-26.04": "e2e-ab12cd-2604",
		"ubuntu-09.90": "e2e-ab12cd-0990",
	} {
		got := clusterName("ab12cd", image)
		if got != want {
			t.Errorf("clusterName(%q) = %q, want %q", image, got, want)
		}
		if !rule.MatchString(got) {
			t.Errorf("cluster name %q breaks tent's name rule", got)
		}
		if !strings.HasPrefix(got, janitor.ClusterPrefix) {
			t.Errorf("cluster name %q does not start with %q", got, janitor.ClusterPrefix)
		}
	}
}

func TestRunnerCIDR(t *testing.T) {
	tests := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{"203.0.113.7", "203.0.113.7/32", false},
		{"203.0.113.7\n", "203.0.113.7/32", false},
		{"2001:db8::1", "2001:db8::1/128", false},
		{"::ffff:203.0.113.7", "203.0.113.7/32", false},
		{"", "", true},
		{"not an address", "", true},
		{"203.0.113.7/24", "", true},
		{"203.0.113.256", "", true},
	}
	for _, tt := range tests {
		got, err := runnerCIDR(tt.addr)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("runnerCIDR(%q) = %q, %v; want %q, error %v", tt.addr, got, err, tt.want, tt.wantErr)
		}
	}
}
