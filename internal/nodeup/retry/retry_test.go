package retry_test

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ingvarch/tent/internal/nodeup/retry"
)

func TestStatus(t *testing.T) {
	for code, want := range map[int]bool{
		http.StatusTooManyRequests: true, http.StatusInternalServerError: true, http.StatusBadGateway: true,
		http.StatusServiceUnavailable: true, http.StatusGatewayTimeout: true, 599: true,
		// 501 says the server will never serve the request.
		http.StatusNotImplemented: false,
		http.StatusOK:             false, http.StatusFound: false, http.StatusForbidden: false, http.StatusNotFound: false,
		600: false,
	} {
		if got := retry.Status(code); got != want {
			t.Errorf("Status(%d) = %t, want %t", code, got, want)
		}
	}
}

func TestSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if !retry.Sleep(t.Context(), 2*time.Second) || time.Since(start) != 2*time.Second {
			t.Errorf("Sleep(2s) ended after %s, or reported false; want true after 2s", time.Since(start))
		}
	})
}

func TestSleepEndsWithTheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		start := time.Now()
		if retry.Sleep(ctx, time.Minute) || time.Since(start) != time.Second {
			t.Errorf("Sleep(1m) ended after %s, or reported true; want false after 1s, when ctx ended", time.Since(start))
		}
	})
}
