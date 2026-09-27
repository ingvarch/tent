package cli

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

// exitInterrupted is tent's exit code when a second signal ends it: 128 plus SIGINT, as shells report a process that
// Ctrl-C ended.
const exitInterrupted = 130

// notifyStopSignals relays the signals that stop tent, Ctrl-C and SIGTERM (Ctrl-C alone on Windows), until stop.
func notifyStopSignals() (sigs <-chan os.Signal, stop func()) {
	c := make(chan os.Signal, 2)
	if runtime.GOOS == "windows" {
		signal.Notify(c, os.Interrupt)
	} else {
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	}
	return c, func() { signal.Stop(c) }
}

// exitProcess ends tent with code.
func exitProcess(code int) { os.Exit(code) }

// executeWithSignals runs tent as Execute does, stopped by the signals from sigs. The first cancels the command's
// context, so that the command can stop and release what it holds; a second calls exit with exitInterrupted. While
// the editor runs, Ctrl-C belongs to the editor.
func executeWithSignals(ctx context.Context, args []string, s Streams, sigs <-chan os.Signal, exit func(int)) int {
	ctx, stop := watchInterrupts(ctx, sigs, exit)
	defer stop()
	return execute(ctx, newRootCommand(s, &globalOptions{}), args, s.Err)
}

// interruptsKey finds the interrupts of a command in its context.
type interruptsKey struct{}

// interrupts tells the goroutine that watches the signals when the editor runs.
type interrupts struct{ editing chan bool }

// watchInterrupts returns a context that the first signal from sigs cancels, and a func that stops watching. A second
// signal calls exit with exitInterrupted. Ctrl-C while whileEditing runs is dropped.
func watchInterrupts(ctx context.Context, sigs <-chan os.Signal, exit func(int)) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	in := interrupts{editing: make(chan bool)}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		editing, cancelled := false, false
		for {
			select {
			case editing = <-in.editing:
			case sig := <-sigs:
				switch {
				case editing && sig == os.Interrupt: // the terminal sent it to the editor too
				case cancelled:
					exit(exitInterrupted)
				default:
					cancelled = true
					cancel()
				}
			case <-done:
				return
			}
		}
	}()
	return context.WithValue(ctx, interruptsKey{}, in), func() {
		close(done)
		<-stopped
		cancel()
	}
}

// whileEditing runs the editor with run. Meanwhile tent drops Ctrl-C and SIGQUIT, as git does: the terminal sends
// them to the editor too, which gives them its own meaning.
func whileEditing(ctx context.Context, run func() error) error {
	if in, ok := ctx.Value(interruptsKey{}).(interrupts); ok {
		in.editing <- true
		defer func() { in.editing <- false }()
	}
	if runtime.GOOS != "windows" {
		quit := make(chan os.Signal, 1) // never read: the signals are dropped
		signal.Notify(quit, syscall.SIGQUIT)
		defer signal.Stop(quit)
	}
	return run()
}
