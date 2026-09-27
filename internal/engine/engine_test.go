package engine

import (
	"fmt"
	"sync"
	"testing"
)

func TestKeyString(t *testing.T) {
	k := Key{Kind: "vultr.FirewallGroup", Name: "prod-servers"}
	if got, want := k.String(), "vultr.FirewallGroup/prod-servers"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestActionString(t *testing.T) {
	for _, tc := range []struct {
		action Action
		want   string
	}{
		{Noop, "noop"},
		{Create, "create"},
		{Update, "update"},
		{Replace, "replace"},
		{Delete, "delete"},
		{Action(7), "Action(7)"},
		{Action(-1), "Action(-1)"},
	} {
		if got := tc.action.String(); got != tc.want {
			t.Errorf("Action(%d).String() = %q, want %q", int(tc.action), got, tc.want)
		}
		text, err := tc.action.MarshalText()
		if err != nil || string(text) != tc.want {
			t.Errorf("Action(%d).MarshalText() = %q, %v, want %q, nil", int(tc.action), text, err, tc.want)
		}
	}
}

func TestOutputsZeroValue(t *testing.T) {
	var o Outputs
	if got, known := o.Get(thing("a"), "id"); got != "" || known {
		t.Errorf("Get on the zero Outputs = %q, %v, want %q, false", got, known, "")
	}
	o.clear(thing("a"))
	o.Set(thing("a"), "id", "1")
	if got, known := o.Get(thing("a"), "id"); got != "1" || !known {
		t.Errorf("Get after Set = %q, %v, want %q, true", got, known, "1")
	}
	o.clear(thing("a"))
	if got, known := o.Get(thing("a"), "id"); got != "" || known {
		t.Errorf("Get after clear = %q, %v, want %q, false", got, known, "")
	}
}

func TestOutputsGet(t *testing.T) {
	o := &Outputs{}
	o.Set(thing("a"), "id", "1")
	o.Set(thing("a"), "id", "2")
	o.Set(thing("a"), "ip", "192.0.2.1")
	o.Set(thing("b"), "id", "3")
	for _, tc := range []struct {
		key   Key
		name  string
		want  string
		known bool
	}{
		{thing("a"), "id", "2", true},
		{thing("a"), "ip", "192.0.2.1", true},
		{thing("b"), "id", "3", true},
		{thing("b"), "ip", "", false},
		{thing("c"), "id", "", false},
	} {
		if got, known := o.Get(tc.key, tc.name); got != tc.want || known != tc.known {
			t.Errorf("Get(%s, %q) = %q, %v, want %q, %v", tc.key, tc.name, got, known, tc.want, tc.known)
		}
	}
}

func TestOutputsClear(t *testing.T) {
	o := &Outputs{}
	o.Set(thing("a"), "id", "1")
	o.Set(thing("a"), "ip", "192.0.2.1")
	o.Set(thing("b"), "id", "2")
	o.clear(thing("a"))
	o.clear(thing("c"))
	for _, tc := range []struct {
		key   Key
		name  string
		want  string
		known bool
	}{
		{thing("a"), "id", "", false},
		{thing("a"), "ip", "", false},
		{thing("b"), "id", "2", true},
	} {
		if got, known := o.Get(tc.key, tc.name); got != tc.want || known != tc.known {
			t.Errorf("Get(%s, %q) = %q, %v, want %q, %v", tc.key, tc.name, got, known, tc.want, tc.known)
		}
	}
	o.Set(thing("a"), "id", "3")
	if got, known := o.Get(thing("a"), "id"); got != "3" || !known {
		t.Errorf("Get after clear and Set = %q, %v, want %q, true", got, known, "3")
	}
}

// TestOutputsConcurrent is for the race detector.
func TestOutputsConcurrent(t *testing.T) {
	o := &Outputs{}
	var wg sync.WaitGroup
	for i := range 8 {
		k := thing(fmt.Sprint(i))
		wg.Go(func() {
			o.Set(k, "id", "1")
			o.Get(k, "id")
			o.clear(k)
			o.Set(k, "id", "2")
		})
	}
	wg.Wait()
	for i := range 8 {
		k := thing(fmt.Sprint(i))
		if got, known := o.Get(k, "id"); got != "2" || !known {
			t.Errorf("Get(%s, id) = %q, %v, want %q, true", k, got, known, "2")
		}
	}
}
