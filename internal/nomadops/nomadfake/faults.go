package nomadfake

import (
	"reflect"
	"slices"
	"testing"

	"github.com/ingvarch/tent/internal/nomadops"
)

// fault changes the outcome of the next call of an API method.
type fault struct {
	call string // the API method
	err  error  // what the call returns without doing anything; nil when the call is done and its answer lost
}

// Fail makes the next call of the nomadops.API method call return err as it is, without doing anything. Faults set
// before for the same method apply first. It fails the test when call is not a method of nomadops.API or err is nil,
// so call it from the test's goroutine.
func (f *Fake) Fail(tb testing.TB, call string, err error) {
	tb.Helper()
	if !checkFault(tb, "Fail", call) {
		return
	}
	if err == nil {
		tb.Fatalf("nomadfake: Fail %s: no error", call)
		return
	}
	f.addFault(fault{call: call, err: err})
}

// LoseResponse makes the next call of the nomadops.API method call lose its answer, as a dropped connection does: the
// fake carries the call out, and the call fails with an error that matches nomadops.ErrNotReady whatever its outcome.
// After a lost bootstrap the ACL system is bootstrapped. Faults set before for the same method apply first. It fails
// the test when call is not a method of nomadops.API, so call it from the test's goroutine.
func (f *Fake) LoseResponse(tb testing.TB, call string) {
	tb.Helper()
	if checkFault(tb, "LoseResponse", call) {
		f.addFault(fault{call: call})
	}
}

// checkFault fails the test and returns false when call is not a method of nomadops.API. setter names the fault
// setter, for the message.
func checkFault(tb testing.TB, setter, call string) bool {
	tb.Helper()
	if _, ok := reflect.TypeFor[nomadops.API]().MethodByName(call); !ok {
		tb.Fatalf("nomadfake: %s: %q is not a method of nomadops.API", setter, call)
		return false
	}
	return true
}

// addFault queues ft after the faults set before.
func (f *Fake) addFault(ft fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, ft)
}

// takeFault removes the first fault for the API method call and returns it. The caller holds the lock.
func (f *Fake) takeFault(call string) (fault, bool) {
	i := slices.IndexFunc(f.faults, func(ft fault) bool { return ft.call == call })
	if i < 0 {
		return fault{}, false
	}
	ft := f.faults[i]
	f.faults = slices.Delete(f.faults, i, i+1)
	return ft, true
}
