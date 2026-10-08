package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// exits records the exit codes that tent asks for.
type exits chan int

func (e exits) exit(code int) { e <- code }

// wantNoExit fails the test if tent asked to exit.
func (e exits) wantNoExit(t *testing.T) {
	t.Helper()
	select {
	case code := <-e:
		t.Errorf("tent exited with %d, want it to go on", code)
	default:
	}
}

// wantExit fails the test unless tent asks to exit with code within a minute.
func (e exits) wantExit(t *testing.T, code int) {
	t.Helper()
	select {
	case got := <-e:
		if got != code {
			t.Errorf("tent exited with %d, want %d", got, code)
		}
	case <-time.After(time.Minute):
		t.Errorf("tent did not exit in a minute, want exit code %d", code)
	}
}

// wantCancelled fails the test unless ctx ends within a minute.
func wantCancelled(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Minute):
		t.Error("the context was not cancelled in a minute")
	}
}

func TestWatchInterrupts(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String()+" cancels, a second exits", func(t *testing.T) {
			sigs, ex := make(chan os.Signal), make(exits, 1)
			ctx, stop := watchInterrupts(t.Context(), sigs, ex.exit)
			defer stop()
			sigs <- sig
			wantCancelled(ctx, t)
			ex.wantNoExit(t)
			sigs <- sig
			ex.wantExit(t, 130)
		})
	}
	t.Run("the editor has Ctrl-C", func(t *testing.T) {
		sigs, ex := make(chan os.Signal), make(exits, 1)
		ctx, stop := watchInterrupts(t.Context(), sigs, ex.exit)
		defer stop()
		_ = whileEditing(ctx, func() error {
			sigs <- os.Interrupt
			sigs <- os.Interrupt
			return nil
		})
		if ctx.Err() != nil {
			t.Error("Ctrl-C while the editor ran cancelled the context")
		}
		ex.wantNoExit(t)
		sigs <- os.Interrupt
		wantCancelled(ctx, t)
	})
	t.Run("SIGTERM stops tent while the editor runs", func(t *testing.T) {
		sigs, ex := make(chan os.Signal), make(exits, 1)
		ctx, stop := watchInterrupts(t.Context(), sigs, ex.exit)
		defer stop()
		_ = whileEditing(ctx, func() error {
			sigs <- syscall.SIGTERM
			return nil
		})
		wantCancelled(ctx, t)
	})
}

// startWithSignals starts tent with args and stdin in another goroutine, stopped by the signals from sigs.
func startWithSignals(t *testing.T, sigs <-chan os.Signal, ex exits, stdin *os.File, args ...string) *started {
	t.Helper()
	return startWithSignalsAndOptions(t, sigs, ex, stdin, args)
}

// startWithSignalsAndOptions starts tent with args, stdin and opts in another goroutine, stopped by the signals from
// sigs.
func startWithSignalsAndOptions(t *testing.T, sigs <-chan os.Signal, ex exits, stdin io.Reader, args []string,
	opts ...Option,
) *started {
	t.Helper()
	r := &started{code: make(chan int, 1)}
	go func() {
		r.code <- executeWithSignals(t.Context(), args, Streams{In: stdin, Out: &r.out, Err: &r.errOut}, sigs, ex.exit,
			opts...)
	}()
	return r
}

// letTheEditorGo waits until the fake editor waits, calls meanwhile, and lets the editor go on.
func letTheEditorGo(t *testing.T, e fakeEditor, meanwhile func()) {
	t.Helper()
	waitUntil(t, "the editor", func() bool { return fileExists(filepath.Join(e.dir, "waiting")) })
	meanwhile()
	if err := os.WriteFile(filepath.Join(e.dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEditKeepsGoingAfterCtrlCInTheEditor, which the terminal sends to the editor too.
func TestEditKeepsGoingAfterCtrlCInTheEditor(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Wait: true})
	sigs, ex := make(chan os.Signal), make(exits, 1)
	r := startWithSignals(t, sigs, ex, stdinFile(t, ""), "edit", "nodegroup", "workers", "--yes", "--name", "prod",
		"--state", s.url)
	letTheEditorGo(t, e, func() { sigs <- os.Interrupt })
	wantDone(t, waitBounded(t, r), biggerWorkersDiff+"node group workers replaced\n")
	ex.wantNoExit(t)
	e.wantNoFile(t)
}

func TestEditCtrlCAtTheQuestion(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	in, noAnswer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = noAnswer.Close(), in.Close() })
	sigs, ex := make(chan os.Signal), make(exits, 1)
	r := startWithSignals(t, sigs, ex, in, "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	waitUntil(t, "the question", func() bool { return r.errOut.String() == "Save? [y/N] " })
	sigs <- os.Interrupt
	got := waitBounded(t, r)
	kept := e.wantKept(t, biggerWorkers(t))
	wantResult(t, got, 1, biggerWorkersDiff,
		"Save? [y/N] \nError: interrupted; nothing saved\n  your edit is in "+kept+"\n")
	ex.wantNoExit(t)
}

// TestEditSecondSignalExits while the editor runs, which tent leaves running.
func TestEditSecondSignalExits(t *testing.T) {
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Wait: true})
	sigs, ex := make(chan os.Signal), make(exits, 1)
	r := startWithSignals(t, sigs, ex, stdinFile(t, ""), "edit", "nodegroup", "workers", "--yes", "--name", "prod",
		"--state", s.url)
	letTheEditorGo(t, e, func() {
		sigs <- syscall.SIGTERM
		sigs <- syscall.SIGTERM
		ex.wantExit(t, 130)
	})
	kept := waitBounded(t, r)
	wantError(t, kept, "Error: interrupted; nothing saved\n  your edit is in "+e.wantKept(t, biggerWorkers(t))+"\n")
}

// TestExecuteDropsSIGQUITInTheEditor, which the terminal sends to the editor too. Unhandled, SIGQUIT would end the
// test binary.
func TestExecuteDropsSIGQUITInTheEditor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGQUIT")
	}
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5", Wait: true})
	r := startExecute(t, stdinFile(t, ""), "edit", "nodegroup", "workers", "--yes", "--name", "prod",
		"--state", s.url)
	letTheEditorGo(t, e, func() { signalSelf(t, syscall.SIGQUIT) })
	wantDone(t, waitBounded(t, r), biggerWorkersDiff+"node group workers replaced\n")
}

// TestExecuteStopsOnARealCtrlC at the question. Unhandled, it would end the test binary.
func TestExecuteStopsOnARealCtrlC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a Windows process cannot send itself Ctrl-C")
	}
	s := withCluster(t)
	e := newFakeEditor(t, "", editStep{Old: "size: 3", New: "size: 5"})
	in, noAnswer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = noAnswer.Close(), in.Close() })
	r := startExecute(t, in, "edit", "nodegroup", "workers", "--name", "prod", "--state", s.url)
	waitUntil(t, "the question", func() bool { return r.errOut.String() == "Save? [y/N] " })
	signalSelf(t, os.Interrupt)
	got := waitBounded(t, r)
	wantResult(t, got, 1, biggerWorkersDiff, "Save? [y/N] \nError: interrupted; nothing saved\n  your edit is in "+
		e.wantKept(t, biggerWorkers(t))+"\n")
}

// startExecute starts tent through Execute, which subscribes to the process's signals, in another goroutine.
func startExecute(t *testing.T, stdin *os.File, args ...string) *started {
	t.Helper()
	r := &started{code: make(chan int, 1)}
	go func() { r.code <- Execute(t.Context(), args, Streams{In: stdin, Out: &r.out, Err: &r.errOut}) }()
	return r
}

// signalSelf sends sig to the test binary.
func signalSelf(t *testing.T, sig os.Signal) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(sig); err != nil {
		t.Fatal(err)
	}
}
