package vultrfake

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ingvarch/tent/internal/cloud/vultr"
)

// faultKind is what a fault does to a call.
type faultKind int

const (
	failCall   faultKind = iota // return an error without doing the call
	loseAnswer                  // do the call and lose its answer
	throttle                    // answer 429 without doing the call
)

// fault changes the outcome of the next calls of an API method.
type fault struct {
	call       string // the API method
	kind       faultKind
	err        error         // what failCall returns
	retryAfter time.Duration // the wait a throttle asks for
	left       int           // how many more calls it applies to
}

// errLost is the cause of the error of a call whose answer a fault lost.
var errLost = errors.New("vultrfake: the answer was lost")

// Fail makes the next times calls of the vultr.API method call return err as it is, without doing anything. It
// fails the test when err is nil.
func (f *Fake) Fail(tb testing.TB, call string, err error, times int) {
	tb.Helper()
	if !checkFault(tb, "Fail", call, times) {
		return
	}
	if err == nil {
		tb.Fatalf("vultrfake: Fail %s: no error", call)
		return
	}
	f.addFault(fault{call: call, kind: failCall, err: err, left: times})
}

// LoseResponse makes the next times calls of the vultr.API method call lose their answer, as a dropped connection
// does: the fake carries the call out, and the call fails with an error that matches vultr.ErrUnavailable whatever
// its outcome. After a lost create the object exists; after a lost delete it is gone.
func (f *Fake) LoseResponse(tb testing.TB, call string, times int) {
	tb.Helper()
	if checkFault(tb, "LoseResponse", call, times) {
		f.addFault(fault{call: call, kind: loseAnswer, left: times})
	}
}

// Throttle makes the next times calls of the vultr.API method call fail without doing anything, with a 429 that
// matches vultr.ErrRateLimited and asks to wait retryAfter. The client sends a GET or DELETE up to four times in all
// when it gets 429s, so for those a throttled call stands for the client giving up. It fails the test when retryAfter
// is negative.
func (f *Fake) Throttle(tb testing.TB, call string, retryAfter time.Duration, times int) {
	tb.Helper()
	if !checkFault(tb, "Throttle", call, times) {
		return
	}
	if retryAfter < 0 {
		tb.Fatalf("vultrfake: Throttle %s: retryAfter is %v, want 0 or more", call, retryAfter)
		return
	}
	f.addFault(fault{call: call, kind: throttle, retryAfter: retryAfter, left: times})
}

// checkFault fails the test and returns false when call is not a vultr.API method or times is below 1. setter names
// the fault setter, for the message.
func checkFault(tb testing.TB, setter, call string, times int) bool {
	tb.Helper()
	if _, ok := reflect.TypeFor[vultr.API]().MethodByName(call); !ok {
		tb.Fatalf("vultrfake: %s: %q is not a method of vultr.API", setter, call)
		return false
	}
	if times < 1 {
		tb.Fatalf("vultrfake: %s %s: times is %d, want 1 or more", setter, call, times)
		return false
	}
	return true
}

// addFault queues ft after the faults set before.
func (f *Fake) addFault(ft fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &ft)
}

// takeFault returns the first fault for the API method call, and counts the call against it. The caller holds the
// lock.
func (f *Fake) takeFault(call string) (fault, bool) {
	i := slices.IndexFunc(f.faults, func(ft *fault) bool { return ft.call == call })
	if i < 0 {
		return fault{}, false
	}
	ft := f.faults[i]
	if ft.left--; ft.left == 0 {
		f.faults = slices.Delete(f.faults, i, i+1)
	}
	return *ft, true
}
