//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ingvarch/tent/test/e2e/vultrapi"
)

// suiteRun is what TestMain sets up for the tests of one run.
type suiteRun struct {
	ID       string
	Dir      string
	Settings settings
	// Runner is the CIDR of the machine that runs the suite.
	Runner string
	Tent   tentRunner
	Vultr  *vultrapi.Client
	// Ctx ends when a signal interrupts the run; the steps' contexts end with it.
	Ctx context.Context
	// SSHKey is the path of the private key; the public key is SSHKey + ".pub".
	SSHKey string
}

// suite is the run the tests belong to; TestMain sets it before any test runs.
var suite suiteRun

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite prepares the run, runs the tests and returns the exit code. An interrupt or SIGTERM ends the contexts of
// the steps, so each test's cleanup deletes its cluster before the run ends.
func runSuite(m *testing.M) int {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	stopAnnounce := announceInterrupt(ctx, os.Stdout)
	defer stopAnnounce()
	s, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}
	suite = s
	fmt.Printf("e2e: run %s in %s on %s, images %s, runner %s, results in %s\n",
		s.ID, s.Settings.Region, s.Settings.Plan, strings.Join(s.Settings.Images, ","), s.Runner, s.Dir)
	return m.Run()
}

// setup checks the environment, makes the run's directory and key pair, checks that the uploaded tent-node is the
// one built with tent and that the Vultr key works, and deletes the leftovers of earlier runs.
func setup(ctx context.Context) (suiteRun, error) {
	cfg, err := readSettings(os.Getenv)
	if err != nil {
		return suiteRun{}, err
	}
	id, err := newRunID(rand.Reader)
	if err != nil {
		return suiteRun{}, err
	}
	root, err := repositoryRoot()
	if err != nil {
		return suiteRun{}, err
	}
	dir := filepath.Join(root, "test", "e2e", "results", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return suiteRun{}, fmt.Errorf("make the run directory: %w", err)
	}
	runner, err := runnerAddress(ctx, cfg.Runner)
	if err != nil {
		return suiteRun{}, err
	}
	keyPath := filepath.Join(dir, "ssh", "id_ed25519")
	if err := makeSSHKey(ctx, keyPath, id); err != nil {
		return suiteRun{}, err
	}
	if err := checkTentNode(cfg.Tent, cfg.NodeSHA); err != nil {
		return suiteRun{}, err
	}
	api := vultrapi.New(cfg.Key)
	api.OnRetry = func(err error, wait time.Duration) { fmt.Printf("e2e: %v; trying again in %s\n", err, wait) }
	if err := preflight(ctx, api); err != nil {
		return suiteRun{}, err
	}
	if err := sweepLeftovers(ctx, api, os.Stdout, time.Now()); err != nil {
		return suiteRun{}, err
	}
	return suiteRun{
		ID: id, Dir: dir, Settings: cfg, Runner: runner,
		Tent: tentRunner{Bin: cfg.Tent, Dir: dir}, Vultr: api, Ctx: ctx, SSHKey: keyPath,
	}, nil
}

// repositoryRoot returns the directory two levels above the package, which must hold go.mod.
func repositoryRoot() (string, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return "", fmt.Errorf("find the repository root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("find the repository root: %w", err)
	}
	return root, nil
}

// runnerAddress returns the CIDR of the runner: the configured address, else the one https://api.ipify.org reports.
func runnerAddress(ctx context.Context, configured string) (string, error) {
	if configured != "" {
		return runnerCIDR(configured)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", fmt.Errorf("ask for the runner address: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask for the runner address (set RUNNER_ADDR to skip it): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", fmt.Errorf("read the runner address: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ask for the runner address: status %d", resp.StatusCode)
	}
	return runnerCIDR(string(body))
}

// makeSSHKey makes an ed25519 key pair without a passphrase at path, in a directory only the user can use.
func makeSSHKey(ctx context.Context, path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("make the ssh directory: %w", err)
	}
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e-"+id, "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("make the ssh key: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// preflight lists each kind of object the suite reads, so a key without the right is found before anything is made.
func preflight(ctx context.Context, api *vultrapi.Client) error {
	for _, kind := range []struct {
		name string
		list func(context.Context) error
	}{
		{"instances", func(ctx context.Context) error { _, err := api.Instances(ctx); return err }},
		{"vpcs", func(ctx context.Context) error { _, err := api.VPCs(ctx); return err }},
		{"firewall groups", func(ctx context.Context) error { _, err := api.FirewallGroups(ctx); return err }},
		{"ssh keys", func(ctx context.Context) error { _, err := api.SSHKeys(ctx); return err }},
	} {
		if err := kind.list(ctx); err != nil {
			return fmt.Errorf("the Vultr key cannot list %s: %w", kind.name, err)
		}
	}
	return nil
}
