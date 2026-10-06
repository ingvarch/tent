// Command tent-node-upload uploads a development build of tent-node to an S3 bucket and prints a presigned URL of it
// and its sha256 as the lines that set TENT_NODE_URL and TENT_NODE_SHA256 in fish or sh, the variables that a
// development build of tent reads. README.md says how to set it up.
package main

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ingvarch/tent/internal/assets"
	"github.com/ingvarch/tent/internal/shellenv"
)

// Exit codes of the tool.
const (
	exitError = 1 // the upload failed
	exitUsage = 2 // the command line is wrong
)

// maxExpires is the longest that a presigned URL works: SigV4 signs for at most 7 days.
const maxExpires = 7 * 24 * time.Hour

// lifecycle is how long an object lives: the bucket's lifecycle rule deletes it 8 days after its upload (README.md).
// The tool uploads an object again when the rule would delete it before the URL expires.
const lifecycle = 8 * 24 * time.Hour

// machines are the architectures of tent-node, with the ELF machine of each.
var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// options are what the command line says.
type options struct {
	binary  string        // the tent-node to upload
	arch    string        // amd64 or arm64
	expires time.Duration // how long the presigned URL works
	shell   string        // shellenv.Sh or shellenv.Fish
}

// usageError is a wrong command line: the tool exits with exitUsage.
type usageError string

func (e usageError) Error() string { return string(e) }

// main runs the tool. Ctrl-C and SIGTERM cancel the upload.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs the tool with args and returns its exit code. Only the two lines go to stdout; the rest goes to stderr.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err == nil {
		err = upload(ctx, o, stdout, stderr)
	}
	if err == nil {
		return 0
	}
	// Nowhere to report a failed write to stderr; the exit code still says the tool failed.
	_, _ = fmt.Fprintf(stderr, "Error: %s\n", err)
	if _, ok := errors.AsType[usageError](err); ok {
		return exitUsage
	}
	return exitError
}

// parseArgs reads the flags. It returns flag.ErrHelp once it has written the usage for -h.
func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("tent-node-upload", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.binary, "binary", "", "the tent-node `file` to upload (default bin/tent-node_linux_<arch>, "+
		"which make build writes for amd64)")
	fs.StringVar(&o.arch, "arch", "amd64", "the `architecture` of the binary: amd64 or arm64")
	fs.DurationVar(&o.expires, "expires", maxExpires, "how long the URL works, from 1s to 168h (7 days)")
	fs.StringVar(&o.shell, "shell", "", "print the lines for fish or sh (default fish when $SHELL is fish, sh "+
		"otherwise)")
	// run writes the errors of the flag package once, as it writes every error.
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.SetOutput(stderr)
		writeUsage(fs)
		return options{}, err
	case err != nil:
		return options{}, usageError(err.Error() + "; run tent-node-upload -h for the flags")
	case fs.NArg() > 0:
		return options{}, usageError("tent-node-upload takes no arguments, only flags")
	}
	if _, ok := machines[o.arch]; !ok {
		return options{}, usageError(fmt.Sprintf("-arch %q: want amd64 or arm64", o.arch))
	}
	if o.expires < time.Second || o.expires > maxExpires {
		return options{}, usageError(fmt.Sprintf("-expires %s: a presigned URL works from 1s to %s (7 days)",
			o.expires, maxExpires))
	}
	if o.shell == "" {
		o.shell = shellenv.Login(os.Getenv("SHELL"))
	}
	if !shellenv.Valid(o.shell) {
		return options{}, usageError(fmt.Sprintf("-shell %q: want fish or sh", o.shell))
	}
	if o.binary == "" {
		o.binary = filepath.Join("bin", assets.TentNodeFile(o.arch))
	}
	return o, nil
}

// writeUsage writes how to run the tool.
func writeUsage(fs *flag.FlagSet) {
	_, _ = fmt.Fprintf(fs.Output(), `Usage: tent-node-upload [flags]

Uploads a tent-node for linux to the bucket in %s, %s,
with the credentials of the AWS environment, unless the bucket holds it already. Then it prints the lines that set
TENT_NODE_URL to a presigned URL of it and TENT_NODE_SHA256 to its sha256, for fish or sh.

Flags:
`, urlEnv, urlForm)
	fs.PrintDefaults()
}

// upload uploads the binary to the bucket in urlEnv, unless the bucket holds it and keeps it until the URL expires,
// and prints the lines that set TENT_NODE_URL to a presigned URL of it and TENT_NODE_SHA256 to its sha256. It sends
// nothing before the bucket URL, the binary and the AWS configuration check out.
func upload(ctx context.Context, o options, stdout, stderr io.Writer) error {
	loc, err := parseBucketURL(os.Getenv(urlEnv))
	if err != nil {
		return err
	}
	f, err := os.Open(o.binary)
	if err != nil {
		return fmt.Errorf("open the tent-node: %w", err)
	}
	defer func() { _ = f.Close() }() // read only
	if err := checkBinary(f, o.arch); err != nil {
		return fmt.Errorf("%s %w", o.binary, err)
	}
	sum, size, err := hashFile(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", o.binary, err)
	}
	b, err := openBucket(ctx, loc)
	if err != nil {
		return err
	}
	until := time.Now().Add(o.expires)
	warning, err := credentialsWarning(ctx, b.credentials(), until)
	if err != nil {
		return err
	}
	if warning != "" {
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
	key := objectKey(loc.Prefix, sum, o.arch)
	where := "s3://" + loc.Bucket + "/" + key
	at, exists, err := b.uploaded(ctx, key)
	if err != nil {
		return fmt.Errorf("look for %s: %w", where, err)
	}
	// An object that the lifecycle rule deletes before the URL expires is uploaded again, which restarts its life.
	if exists && until.Before(at.Add(lifecycle)) {
		_, _ = fmt.Fprintf(stderr, "%s exists: not uploaded again\n", where)
	} else {
		if exists {
			_, _ = fmt.Fprintf(stderr, "%s exists, but the lifecycle rule deletes it before the URL expires: "+
				"uploading it again\n", where)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("read %s: %w", o.binary, err)
		}
		if err := b.put(ctx, key, f, size); err != nil {
			return fmt.Errorf("upload to %s: %w", where, err)
		}
		_, _ = fmt.Fprintf(stderr, "uploaded %s (%d bytes)\n", where, size)
	}
	nodeURL, err := b.presign(ctx, key, o.expires)
	if err != nil {
		return fmt.Errorf("presign a download of %s: %w", where, err)
	}
	_, _ = fmt.Fprintf(stderr, "the URL works until %s\n", until.UTC().Format(time.RFC3339))
	_, err = fmt.Fprintf(stdout, "%s\n%s\n", shellenv.ExportLine(o.shell, "TENT_NODE_URL", nodeURL),
		shellenv.ExportLine(o.shell, "TENT_NODE_SHA256", sum))
	return err
}

// objectKey returns the key of tent-node for linux on arch with the sha256 sum: below the prefix, in a directory of
// its own sha256, by the name that tent's releases give it.
func objectKey(prefix, sum, arch string) string {
	return prefix + "/tent-node/" + sum + "/" + assets.TentNodeFile(arch)
}

// checkBinary checks that f is a 64-bit linux ELF executable for arch, as its header says. A node of another
// architecture fails to run it, and so does a node given a 32-bit binary, an object file or a build for another OS.
func checkBinary(f io.ReaderAt, arch string) error {
	e, err := elf.NewFile(f)
	if err != nil {
		return errors.New("is not a linux executable: it has no ELF header")
	}
	// Go leaves the OS ABI of a linux build at ELFOSABI_NONE; GNU ld writes ELFOSABI_LINUX for GNU extensions.
	if e.OSABI != elf.ELFOSABI_NONE && e.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("is not a linux executable: its OS ABI is %s", e.OSABI)
	}
	if e.Class != elf.ELFCLASS64 {
		return fmt.Errorf("is not a 64-bit executable: its class is %s", e.Class)
	}
	// A position-independent executable, as -buildmode=pie builds, has the type ET_DYN.
	if e.Type != elf.ET_EXEC && e.Type != elf.ET_DYN {
		return fmt.Errorf("is not an executable: its type is %s", e.Type)
	}
	if e.Machine == machines[arch] {
		return nil
	}
	for other, m := range machines {
		if e.Machine == m {
			return fmt.Errorf("is for %s, not %s", other, arch)
		}
	}
	return fmt.Errorf("is for %s, not %s", e.Machine, arch)
}

// hashFile returns the sha256 of f from its start, in lower-case hex, and its size.
func hashFile(f io.ReadSeeker) (sum string, size int64, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	if size, err = io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
