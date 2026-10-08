package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr/vultrfake"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/statestore"
)

// oneZoneWarning is the warning about the test cluster, whose machines all run in one region, as validate prints it.
const oneZoneWarning = "WARNING: cluster prod runs in one failure domain, ams: an outage there takes the whole " +
	"cluster down\n"

// validCluster is what validate cluster prints on stdout for the test cluster that an update built.
const validCluster = "cluster prod is valid: 3 servers and 3 clients run Nomad 2.0.7\n"

// validateArgs returns the arguments that validate the test cluster in the store s, followed by more.
func validateArgs(s state, more ...string) []string {
	return append([]string{"validate", "cluster", "prod", "--state", s.url}, more...)
}

// withoutWorker deletes the machine of the worker prod-workers-2 from the fake, which the update gave the ID
// instance-6: the cluster then has 2 of the 3 workers that its node group asks for.
func withoutWorker(t *testing.T, f *vultrfake.Fake) {
	t.Helper()
	if err := f.DeleteInstance(t.Context(), "instance-6"); err != nil {
		t.Fatal(err)
	}
}

// missingWorker is what validate cluster prints on stdout for the test cluster without one of its workers.
const missingWorker = "NODE            FAILURE\n" +
	"prod-workers-2  no machine: node group workers has 2 of its 3\n" +
	"\n" +
	"cluster prod is not valid: 1 failure\n"

// lateNode returns the Nomad factory of staticNomad in which the node called name is listed only once delay has
// passed since the call, on the clock of the test's synctest bubble.
func lateNode(name string, delay time.Duration) func(nomadops.Config) (nomadops.API, error) {
	inner, start := staticNomad(), time.Now()
	return func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return &timedAPI{API: api, name: name, until: start.Add(delay)}, err
	}
}

// timedAPI is a nomadops.API that lists the node called name only from the time until.
type timedAPI struct {
	nomadops.API
	name  string
	until time.Time
}

func (a *timedAPI) Nodes(ctx context.Context) ([]nomadops.Node, error) {
	nodes, err := a.API.Nodes(ctx)
	if time.Now().Before(a.until) {
		return slices.DeleteFunc(nodes, func(n nomadops.Node) bool { return n.Name == a.name }), err
	}
	return nodes, err
}

// countRounds returns the Nomad factory inner and the number of rounds that ask it: a round asks for the leader once.
func countRounds(
	inner func(nomadops.Config) (nomadops.API, error),
) (func(nomadops.Config) (nomadops.API, error), *int) {
	var rounds int
	return func(cfg nomadops.Config) (nomadops.API, error) {
		api, err := inner(cfg)
		return &countingAPI{API: api, rounds: &rounds}, err
	}, &rounds
}

// countingAPI is a nomadops.API that counts the calls of Leader.
type countingAPI struct {
	nomadops.API
	rounds *int
}

func (a *countingAPI) Leader(ctx context.Context) (string, error) {
	*a.rounds++
	return a.API.Leader(ctx)
}

// notRegistered is the Nomad that lists no worker prod-workers-2, ever.
func notRegistered() func(nomadops.Config) (nomadops.API, error) {
	return lateNode("prod-workers-2", 24*time.Hour)
}

// notRegisteredTable is what validate cluster prints on stdout for the test cluster whose worker prod-workers-2 is
// not registered in Nomad.
const notRegisteredTable = "NODE            FAILURE\n" +
	"prod-workers-2  Nomad lists no client of its name at 10.64.0.8\n" +
	"\n" +
	"cluster prod is not valid: 1 failure\n"

// notValidYet is the notice that a wait prints once for the test cluster, for the count of the first round's failures,
// such as "1 failure", and a wait of the given text.
func notValidYet(count, wait string) string {
	return "cluster prod is not valid yet (" + count + "); checking every 10s for up to " + wait + "\n"
}

// TestValidateClusterValid prints that the built cluster is valid, with the warnings that every run prints on stderr.
func TestValidateClusterValid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		before := len(f.Calls())

		got := runOn(t, f, validateArgs(s)...)

		wantResult(t, got, 0, validCluster, openAPIWarning+oneZoneWarning)
		wantNoWritesIn(t, f.Calls()[before:])
	})
}

// TestValidateClusterNotValid prints the table of the failures and exits with 2, without an error line.
func TestValidateClusterNotValid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		withoutWorker(t, f)

		got := runOn(t, f, validateArgs(s)...)

		wantResult(t, got, 2, missingWorker, openAPIWarning+oneZoneWarning)
	})
}

// TestValidateClusterNomadFailure exits with 2 for a failure that only Nomad shows.
func TestValidateClusterNomadFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got := runWithNomad(t, onVultr(f), notRegistered(), validateArgs(s)...)

		wantResult(t, got, 2, notRegisteredTable, openAPIWarning+oneZoneWarning)
	})
}

// TestValidateClusterJSON prints the result as the use case encodes it, for a valid and for a failing cluster; the
// exit code does not depend on the format.
func TestValidateClusterJSON(t *testing.T) {
	type failure struct {
		Check  string `json:"check"`
		Node   string `json:"node"`
		ID     string `json:"id"`
		Detail string `json:"detail"`
	}
	type output struct {
		Cluster      string    `json:"cluster"`
		Valid        bool      `json:"valid"`
		Servers      int       `json:"servers"`
		Clients      int       `json:"clients"`
		NomadVersion string    `json:"nomadVersion"`
		Failures     []failure `json:"failures"`
		Warnings     []string  `json:"warnings"`
		Lock         *struct{} `json:"lock"`
	}
	warnings := []string{
		strings.TrimSuffix(strings.TrimPrefix(openAPIWarning, "WARNING: "), "\n"),
		strings.TrimSuffix(strings.TrimPrefix(oneZoneWarning, "WARNING: "), "\n"),
	}
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		for _, c := range []struct {
			name  string
			nomad func(nomadops.Config) (nomadops.API, error)
			code  int
			want  output
		}{
			{"valid", staticNomad(), 0, output{
				Cluster: "prod", Valid: true, Servers: 3, Clients: 3, NomadVersion: "2.0.7", Failures: []failure{},
				Warnings: warnings,
			}},
			{"failing", notRegistered(), 2, output{
				Cluster: "prod", Servers: 3, Clients: 3, NomadVersion: "2.0.7", Warnings: warnings,
				Failures: []failure{{
					Check: "client-not-registered", Node: "prod-workers-2", ID: "instance-6",
					Detail: "Nomad lists no client of its name at 10.64.0.8",
				}},
			}},
		} {
			got := runWithNomad(t, onVultr(f), c.nomad, validateArgs(s, "-o", "json")...)
			var decoded output
			if err := json.Unmarshal([]byte(got.out), &decoded); err != nil {
				t.Fatalf("%s: stdout is no JSON: %v\n%s", c.name, err, got.out)
			}
			if got.code != c.code || got.errOut != openAPIWarning+oneZoneWarning {
				t.Errorf("%s: exit code = %d, stderr\n%s\nwant %d and the two warnings", c.name, got.code, got.errOut, c.code)
			}
			if diff := cmp.Diff(c.want, decoded); diff != "" {
				t.Errorf("%s: result (-want +got):\n%s", c.name, diff)
			}
			if c.want.Valid && !strings.Contains(got.out, `"failures": []`) {
				t.Errorf("%s: failures are not an empty list in\n%s", c.name, got.out)
			}
		}
	})
}

// TestValidateClusterYAML prints the result as YAML and exits with 2 for a cluster that is not valid.
func TestValidateClusterYAML(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)

		got := runWithNomad(t, onVultr(f), notRegistered(), validateArgs(s, "-o", "yaml")...)

		var decoded struct {
			Valid    bool
			Failures []struct{ Check string }
		}
		if err := yaml.Unmarshal([]byte(got.out), &decoded); err != nil {
			t.Fatalf("stdout is no YAML: %v\n%s", err, got.out)
		}
		if got.code != 2 || decoded.Valid || len(decoded.Failures) != 1 ||
			decoded.Failures[0].Check != "client-not-registered" {
			t.Errorf("exit code = %d, result\n%s\nwant 2 and one client-not-registered", got.code, got.out)
		}
		if strings.Contains(got.errOut, "Error:") {
			t.Errorf("stderr holds an error line:\n%s", got.errOut)
		}
	})
}

// TestValidateClusterWaitsUntilValid checks every 10 seconds: a worker that registers after 30 seconds ends the wait
// with code 0, after one notice, and the result that stdout shows is the last round's. Each warning is printed once
// although every round tells it.
func TestValidateClusterWaitsUntilValid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		start := time.Now()

		got := runWithNomad(t, onVultr(f), lateNode("prod-workers-2", 30*time.Second),
			validateArgs(s, "--wait", "1m")...)

		wantResult(t, got, 0, validCluster, openAPIWarning+oneZoneWarning+notValidYet("1 failure", "1m0s"))
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Errorf("validate waited %s, want 30s: the round at 30s finds the worker", elapsed)
		}
	})
}

// TestValidateClusterWaitTimesOut checks until the wait has passed, and then prints the last round's table and exits
// with 2: the rounds are at 0s, 10s, 20s and 25s for a wait of 25 seconds, and the last wait is the 5 seconds that
// are left. The notice counts the two failures of the first round: prod-workers-2 has no machine, and Nomad does not
// list prod-workers-1.
func TestValidateClusterWaitTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		withoutWorker(t, f)
		start := time.Now()
		nomad, rounds := countRounds(lateNode("prod-workers-1", 24*time.Hour))

		got := runWithNomad(t, onVultr(f), nomad, validateArgs(s, "--wait", "25s")...)

		wantResult(t, got, 2, "NODE            FAILURE\n"+
			"prod-workers-2  no machine: node group workers has 2 of its 3\n"+
			"prod-workers-1  Nomad lists no client of its name at 10.64.0.7\n"+
			"\n"+
			"cluster prod is not valid: 2 failures\n",
			openAPIWarning+oneZoneWarning+notValidYet("2 failures", "25s"))
		if elapsed := time.Since(start); elapsed != 25*time.Second {
			t.Errorf("validate waited %s, want 25s", elapsed)
		}
		if *rounds != 4 {
			t.Errorf("validate made %d rounds, want 4 (at 0s, 10s, 20s and 25s)", *rounds)
		}
	})
}

// TestValidateClusterWithoutWaitChecksOnce checks once and prints no notice, with no --wait and with --wait 0s.
func TestValidateClusterWithoutWaitChecksOnce(t *testing.T) {
	for name, more := range map[string][]string{"no flag": nil, "wait 0s": {"--wait", "0s"}} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)
				start := time.Now()
				nomad, rounds := countRounds(notRegistered())

				got := runWithNomad(t, onVultr(f), nomad, validateArgs(s, more...)...)

				wantResult(t, got, 2, notRegisteredTable, openAPIWarning+oneZoneWarning)
				if elapsed := time.Since(start); elapsed != 0 || *rounds != 1 {
					t.Errorf("validate made %d rounds in %s, want 1 round at once", *rounds, elapsed)
				}
			})
		})
	}
}

// errClosedStdout is the error of a stdout that cannot be written.
var errClosedStdout = errors.New("stdout is closed")

// closedWriter is a writer that fails on every write.
type closedWriter struct{}

func (closedWriter) Write([]byte) (int, error) { return 0, errClosedStdout }

// TestValidateClusterFailsWhenTheResultCannotBeWritten exits with 1, and not with 0 or 2, when stdout fails, for a
// valid and for a not valid cluster, in each output format, and ends stderr with the error of that format's writer.
func TestValidateClusterFailsWhenTheResultCannotBeWritten(t *testing.T) {
	for format, failed := range map[string]string{
		"table": "writing the result", "json": "encoding JSON", "yaml": "writing YAML",
	} {
		// The factories are made inside the bubble: one of them reads the clock when it is made.
		for name, nomad := range map[string]func() func(nomadops.Config) (nomadops.API, error){
			"valid": staticNomad, "not valid": notRegistered,
		} {
			t.Run(format+" "+name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s, f := builtCluster(t)
					var errOut syncBuffer

					code := executeTest(t.Context(), t, validateArgs(s, "-o", format),
						Streams{In: strings.NewReader(""), Out: closedWriter{}, Err: &errOut},
						WithProviders(onVultr(f)), WithAssets(testAssets()), WithNomad(nomad()))

					want := openAPIWarning + oneZoneWarning + "Error: " + failed + ": " + errClosedStdout.Error() + "\n"
					if code != 1 || errOut.String() != want {
						t.Errorf("exit code = %d, stderr\n%s\nwant 1 and\n%s", code, errOut.String(), want)
					}
				})
			})
		}
	}
}

// lateReads is a store that counts the reads that come with a context that has ended.
type lateReads struct {
	statestore.Store
	count *int
}

func (s lateReads) Get(ctx context.Context, p string) ([]byte, statestore.Version, error) {
	s.see(ctx)
	return s.Store.Get(ctx, p)
}

func (s lateReads) List(ctx context.Context, prefix string) ([]string, error) {
	s.see(ctx)
	return s.Store.List(ctx, prefix)
}

// see counts a read whose context ctx has ended.
func (s lateReads) see(ctx context.Context) {
	if ctx.Err() != nil {
		*s.count++
	}
}

// TestValidateClusterInterruptedWhileWaiting ends a wait on Ctrl-C at once, with the error interrupted and code 1. It
// prints no result and starts no round after the Ctrl-C: the store gets no read then.
func TestValidateClusterInterruptedWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		start := time.Now()
		ctx, interrupt := context.WithCancel(t.Context())
		go func() {
			time.Sleep(15 * time.Second)
			interrupt()
		}()
		var out, errOut syncBuffer
		var late int
		counting := func(st statestore.Store) statestore.Store { return lateReads{Store: st, count: &late} }
		opts := &globalOptions{
			openStore: wrapped(counting), providers: onVultr(f), assets: testAssets(), nomad: notRegistered(),
		}

		code := execute(ctx, newRootCommand(Streams{Out: &out, Err: &errOut}, opts), validateArgs(s, "--wait", "10m"),
			&errOut)

		if code != 1 || out.String() != "" ||
			errOut.String() != openAPIWarning+oneZoneWarning+notValidYet("1 failure", "10m0s")+"Error: interrupted\n" {
			t.Errorf("exit code = %d, stdout\n%s\nstderr\n%s\nwant 1, no stdout and the notice and Error: interrupted",
				code, out.String(), errOut.String())
		}
		if elapsed := time.Since(start); elapsed != 15*time.Second || late != 0 {
			t.Errorf("validate ended after %s, with %d reads of the store after the Ctrl-C; want 15s and none",
				elapsed, late)
		}
	})
}

// TestValidateClusterWaitStopsOnAnError ends a wait with code 1 and an error line, and no result, when a later round
// cannot check: the Nomad client of the second round cannot be made.
func TestValidateClusterWaitStopsOnAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		inner, calls := notRegistered(), 0
		nomad := func(cfg nomadops.Config) (nomadops.API, error) {
			if calls++; calls > 3 { // the first round makes one client for each of the 3 servers
				return nil, errors.New("no Nomad client")
			}
			return inner(cfg)
		}

		got := runWithNomad(t, onVultr(f), nomad, validateArgs(s, "--wait", "1m")...)

		wantResult(t, got, 1, "", openAPIWarning+oneZoneWarning+notValidYet("1 failure", "1m0s")+
			"Error: reach server prod-servers-0: no Nomad client\n")
	})
}

// TestValidateClusterPrintsTheCombinedWarning warns of a combined group on every run, after the warning of the open
// API.
func TestValidateClusterPrintsTheCombinedWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := newState(t), vultrfake.New()
		if got := runWithNomad(t, onVultr(f), combinedNomad(), createProd(s, "--combined", "--yes")...); got.code != 0 {
			t.Fatalf("create --yes: exit code %d\n%s", got.code, got.errOut)
		}

		got := runWithNomad(t, onVultr(f), combinedNomad(), validateArgs(s)...)

		wantResult(t, got, 0, validCluster, openAPIWarning+combinedWarning+oneZoneWarning)
	})
}

// TestValidateClusterNoticesTheLock says who holds the lock, on stderr, and the lock is no failure: the result is
// valid and the code 0. The lock stays held.
func TestValidateClusterNoticesTheLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		lock := holdLock(t, s)

		got := runOn(t, f, validateArgs(s)...)

		wantResult(t, got, 0, validCluster,
			openAPIWarning+oneZoneWarning+"cluster prod is locked by "+lock.Lease().String()+"\n")
		if held := runOn(t, f, validateArgs(s, "-o", "json")...); !strings.Contains(held.out, `"lock": {`) {
			t.Errorf("the JSON result does not hold the lock:\n%s", held.out)
		}
	})
}

// TestValidateClusterNoticesTheLastLock tells of the lock that the last round found: a lock held at the first round and
// released before the last is not told.
func TestValidateClusterNoticesTheLastLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		lock := holdLock(t, s)
		go func() {
			time.Sleep(15 * time.Second)
			if err := lock.Release(context.Background()); err != nil {
				t.Errorf("Release: %v", err)
			}
		}()

		got := runWithNomad(t, onVultr(f), lateNode("prod-workers-2", 20*time.Second), validateArgs(s, "--wait", "1m")...)

		wantResult(t, got, 0, validCluster, openAPIWarning+oneZoneWarning+notValidYet("1 failure", "1m0s"))
	})
}

// TestValidateClusterErrors fails with code 1 and an error line, and prints no result, when tent could not check.
func TestValidateClusterErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		noKey := errors.New("VULTR_API_KEY is not set")

		wantError(t, runProviders(t, func(v1alpha1.Provider, *slog.Logger) (cloud.Provider, error) { return nil, noKey },
			validateArgs(s)...), "Error: VULTR_API_KEY is not set\n")
		wantError(t, runWith(t, &globalOptions{providers: onVultr(f), assets: testAssets()}, validateArgs(s)...),
			"Error: no Nomad client is set up\n")

		s.put(t, "prod/pki/ca-bundle.pem", "not a certificate")
		wantError(t, runOn(t, f, validateArgs(s)...),
			"Error: prod/pki/private/ca.key and prod/pki/ca-bundle.pem: CA bundle: no certificates\n")
	})
}

// TestValidateClusterRejectsANegativeWait fails before it opens the store, as --lock-timeout does.
func TestValidateClusterRejectsANegativeWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		before := len(f.Calls())

		wantError(t, runOn(t, f, validateArgs(s, "--wait", "-5s")...), "Error: invalid --wait -5s: must not be negative\n")
		if n := len(f.Calls()) - before; n != 0 {
			t.Errorf("the command made %d cloud calls, want none", n)
		}
	})
}

// TestValidateClusterRejectsASingleServer fails the specs as update does, unless --allow-single-server is given.
func TestValidateClusterRejectsASingleServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := builtCluster(t)
		s.put(t, serversPath, replaced(t, serversYAML, "size: 3", "size: 1"))

		got := runOn(t, f, validateArgs(s)...)
		if got.code != 1 || got.out != "" || !strings.Contains(got.errOut, "Error: invalid spec:") {
			t.Errorf("exit code = %d, stdout\n%s\nstderr\n%s\nwant 1, an invalid spec and no result", got.code, got.out,
				got.errOut)
		}
		// The specs load now; the cloud still has three servers, so the cluster is not valid.
		allowed := runOn(t, f, validateArgs(s, "--allow-single-server")...)
		if allowed.code != 2 || strings.Contains(allowed.errOut, "Error:") {
			t.Errorf("--allow-single-server: exit code %d, stderr\n%s\nwant 2 and no error", allowed.code, allowed.errOut)
		}
	})
}

// TestValidateClusterNeedsAStateAndAName fails without a state store and without a cluster name.
func TestValidateClusterNeedsAStateAndAName(t *testing.T) {
	s := withCluster(t)

	if got := runOnCloud(t, "validate", "cluster", "prod"); got.code != 1 ||
		!strings.Contains(got.errOut, "Error: no state store: set --state, TENT_STATE or ") {
		t.Errorf("without a state: exit code = %d, stderr\n%s", got.code, got.errOut)
	}
	if got := runOnCloud(t, "validate", "cluster", "--state", s.url); got.code != 1 ||
		!strings.Contains(got.errOut, "Error: no cluster name: give NAME or set --name") {
		t.Errorf("without a name: exit code = %d, stderr\n%s", got.code, got.errOut)
	}
}

// TestValidateClusterTakesOneName fails for two names.
func TestValidateClusterTakesOneName(t *testing.T) {
	wantError(t, runOnCloud(t, "validate", "cluster", "prod", "dev"), "Error: accepts at most 1 arg(s), received 2\n")
}

// TestValidateClusterShowsNoSecrets prints no secret that the update wrote, in any format, with debug logs, for a valid
// and a failing cluster.
func TestValidateClusterShowsNoSecrets(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f := builtCluster(t)
				for _, tc := range []struct {
					nomad func(nomadops.Config) (nomadops.API, error)
					code  int
				}{{staticNomad(), 0}, {notRegistered(), 2}} {
					got := runWithNomad(t, onVultr(f), tc.nomad, validateArgs(s, "-o", format, "-vv")...)
					if got.code != tc.code {
						t.Fatalf("exit code = %d, want %d; stderr:\n%s", got.code, tc.code, got.errOut)
					}
					wantNoSecrets(t, s, "stdout", got.out)
					wantNoSecrets(t, s, "stderr", got.errOut)
				}
			})
		})
	}
}

// TestValidateHelp says what is checked, the exit codes and what the command needs.
func TestValidateHelp(t *testing.T) {
	got := runOnCloud(t, "validate", "cluster", "--help")
	for _, want := range []string{
		"Usage:\n  tent validate cluster [NAME] [flags]\n", "--wait", "--allow-single-server", "VULTR_API_KEY",
		"port 4646", "exits with 0 when the cluster is valid", ", with 2 when it is not",
		", and with 1 when tent could not check",
		"It checks that each node group has its machines, that the cloud reports them as running, that each has " +
			"joined Nomad, that Nomad has a leader, that the servers vote, are alive and healthy, that the clients are " +
			"registered, ready and eligible, that every node runs the pinned Nomad version, and that no certificate " +
			"has ended.",
	} {
		if got.code != 0 || !strings.Contains(got.out, want) {
			t.Errorf("exit code = %d, stdout\n%s\nwant 0 and it to hold %q", got.code, got.out, want)
		}
	}
	if group := runOnCloud(t, "validate"); group.code != 0 || !strings.Contains(group.out, "cluster") {
		t.Errorf("validate alone: exit code = %d, stdout\n%s\nwant 0 and its help", group.code, group.out)
	}
}

// TestValidateClusterWarnsAboutOutdatedNodes warns on stderr about the nodes that tent rolling-update cluster replaces,
// and finds the cluster valid.
func TestValidateClusterWarnsAboutOutdatedNodes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f := outdatedCluster(t)

		got := runWithNomad(t, onVultr(f), staticNomad(), validateArgs(s)...)

		const warning = "WARNING: 3 nodes are outdated: prod-workers-0, prod-workers-1 and prod-workers-2; " +
			"tent rolling-update cluster replaces them\n"
		wantResult(t, got, 0, "cluster prod is valid: 3 servers and 3 clients run Nomad 2.0.7\n",
			openAPIWarning+oneZoneWarning+warning)
	})
}
