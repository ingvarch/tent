package vultr

import "testing"

func TestOSID(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  int
		ok    bool
	}{
		{"ubuntu-24.04", 2284, true},
		{"ubuntu-26.04", 2760, true},
		{"debian-12", 0, false},
		{"Ubuntu-24.04", 0, false},
		{"", 0, false},
	} {
		if got, ok := osID(tc.image); got != tc.want || ok != tc.ok {
			t.Errorf("osID(%q) = %d, %t; want %d, %t", tc.image, got, ok, tc.want, tc.ok)
		}
	}
}

func TestImageNames(t *testing.T) {
	if got, want := imageNames(), "ubuntu-24.04 and ubuntu-26.04"; got != want {
		t.Errorf("imageNames() = %q, want %q", got, want)
	}
}
