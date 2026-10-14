package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ingvarch/tent/internal/statestore"
)

// emptyConfigDir is an empty XDG_CONFIG_HOME that the tests share, so that the user's own config file never reaches a
// test. TestMain sets it.
var emptyConfigDir string

// testEnv is the environment of the commands in one test and its subtests: what t.Setenv would set, without the
// process's environment, which parallel tests share.
type testEnv struct {
	mu      sync.Mutex
	vars    map[string]string
	tempDir string
}

// testEnvs holds the environment that a test has set, by the name of the test.
var testEnvs sync.Map

// ancestorEnv returns the environment of the test that has the longest name that is name or a parent of name, or nil.
func ancestorEnv(name string) *testEnv {
	var best *testEnv
	longest := -1
	testEnvs.Range(func(k, v any) bool {
		key := k.(string)
		if (name == key || strings.HasPrefix(name, key+"/")) && len(key) > longest {
			best, longest = v.(*testEnv), len(key)
		}
		return true
	})
	return best
}

// envOf returns the environment of t: the one that t or its nearest parent has set, else a new default that sets only
// XDG_CONFIG_HOME, to the empty config directory, and has no home directory.
func envOf(t *testing.T) *testEnv {
	t.Helper()
	if e := ancestorEnv(t.Name()); e != nil {
		return e
	}
	return &testEnv{vars: map[string]string{"XDG_CONFIG_HOME": emptyConfigDir}}
}

// ownEnv returns the environment that belongs to t alone, a copy of its parent's the first time, which t.Cleanup
// removes when t ends.
func ownEnv(t *testing.T) *testEnv {
	t.Helper()
	if e, ok := testEnvs.Load(t.Name()); ok {
		return e.(*testEnv)
	}
	parent := envOf(t)
	parent.mu.Lock()
	own := &testEnv{vars: make(map[string]string, len(parent.vars)+1), tempDir: parent.tempDir}
	for k, v := range parent.vars {
		own.vars[k] = v
	}
	parent.mu.Unlock()
	testEnvs.Store(t.Name(), own)
	t.Cleanup(func() { testEnvs.Delete(t.Name()) })
	return own
}

// setenv sets a variable of the commands that t and its subtests run. It does what t.Setenv does, but it runs in a
// parallel test.
func setenv(t *testing.T, key, value string) {
	t.Helper()
	e := ownEnv(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[key] = value
}

// setTempDir sets where the commands that t and its subtests run make their temporary files.
func setTempDir(t *testing.T, dir string) {
	t.Helper()
	e := ownEnv(t)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tempDir = dir
}

func (e *testEnv) get(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.vars[key]
}

// errNoHome is the error of a test environment that has no HOME.
var errNoHome = errors.New("no home directory in the test environment")

// home is the variable HOME, an error when it is empty.
func (e *testEnv) home() (string, error) {
	if h := e.get("HOME"); h != "" {
		return h, nil
	}
	return "", errNoHome
}

func (e *testEnv) dir() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tempDir
}

// openWithoutSync opens the store at a URL as statestore.Open does, but skips its syncs: a test does not need the
// store to survive a crash.
func openWithoutSync(ctx context.Context, url string) (statestore.Store, error) {
	return statestore.OpenWith(ctx, url, statestore.WithoutSync())
}

// optionsOf returns the options of the commands that t runs, as testOptions makes them.
func optionsOf(t *testing.T) *globalOptions {
	t.Helper()
	o := &globalOptions{}
	testOptions(t)(o)
	return o
}

// testOptions returns the Option that gives the commands of t the environment of t and a state store without syncs. A
// setting that other options have made stays.
func testOptions(t *testing.T) Option {
	t.Helper()
	e := envOf(t)
	return func(o *globalOptions) {
		if o.getenv == nil {
			o.getenv, o.homeDir = e.get, e.home
		}
		if o.tempDir == "" {
			o.tempDir = e.dir()
		}
		if o.openStore == nil {
			o.openStore = openWithoutSync
		}
	}
}
