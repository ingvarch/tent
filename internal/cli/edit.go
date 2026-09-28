package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/spec"
)

func newEditCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("edit", "Edit a spec in an editor")
	cmd.AddCommand(newEditClusterCommand(opts), newEditNodeGroupCommand(opts))
	return cmd
}

// editHelp says how edit works, after what it edits.
const editHelp = " in an editor: $TENT_EDITOR, $VISUAL or $EDITOR, else vi (notepad on Windows). The editor must " +
	"not return until the file is closed, as with code --wait. When the editor exits, tent checks the spec and " +
	"opens the editor again with the errors at the top until the spec is valid; save it unchanged to cancel, and " +
	"tent keeps your edit. Then it prints a diff and asks before it saves. Nothing changes in the cloud."

func newEditClusterCommand(opts *globalOptions) *cobra.Command {
	var f editFlags
	cmd := &cobra.Command{
		Use:   "cluster [NAME]",
		Short: "Edit the Cluster spec of a cluster",
		Long:  "Edit the Cluster named by NAME or --name" + editHelp,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			return editSpec(cmd, opts, f, name, v1alpha1.KindCluster, "")
		},
	}
	f.add(cmd)
	return cmd
}

func newEditNodeGroupCommand(opts *globalOptions) *cobra.Command {
	var f editFlags
	cmd := &cobra.Command{
		Use:   "nodegroup NAME",
		Short: "Edit the spec of a node group",
		Long:  "Edit the node group NAME of the cluster named by --name, TENT_CLUSTER or the config file" + editHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, err := opts.requireCluster()
			if err != nil {
				return err
			}
			return editSpec(cmd, opts, f, cluster, v1alpha1.KindNodeGroup, args[0])
		},
	}
	f.add(cmd)
	return cmd
}

// editFlags are the flags of the edit commands.
type editFlags struct {
	yes         bool
	allowSingle bool
}

func (f *editFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.yes, "yes", false, "save without asking")
	addAllowSingleServer(cmd, &f.allowSingle)
}

// editSpec lets the operator edit one object of a cluster in the editor, and saves it.
func editSpec(cmd *cobra.Command, opts *globalOptions, f editFlags, cluster, kind, name string) error {
	// The output format of the config file is a default for the commands that print data; edit prints text.
	if cmd.Flags().Changed("output") && opts.output != outputTable {
		return errors.New("edit is interactive and prints text; -o json and -o yaml are not supported")
	}
	svc, err := opts.service(cmd, v1alpha1.ValidateOptions{AllowSingleServer: f.allowSingle})
	if err != nil {
		return err
	}
	svc.OnWarning = warnOnce(cmd.ErrOrStderr())
	objs, ref, err := svc.Load(cmd.Context(), cluster, kind, name)
	if err != nil {
		return err
	}
	stored, err := spec.Encode(objs)
	if err != nil {
		return err
	}
	e, err := newEditing(cmd, svc, ref, stored)
	if err != nil {
		return err
	}
	return e.finish(e.run(f.yes))
}

// editing is one run of edit: the object loaded and the file that the operator edits.
type editing struct {
	cmd    *cobra.Command
	svc    *app.Service
	ref    app.Ref
	path   string
	stored []byte // the object as loaded, which the file holds at first
	header []byte // the errors that the file shows above the operator's text, once a check has failed
	text   []byte // the operator's text in the file
	saved  bool   // the edited object is saved
	keep   bool   // the file stays, for the operator's text
}

// newEditing writes the stored object to a new temporary file for the operator to edit.
func newEditing(cmd *cobra.Command, svc *app.Service, ref app.Ref, stored []byte) (*editing, error) {
	f, err := os.CreateTemp("", "tent-edit-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("creating the file to edit: %w", err)
	}
	_, err = f.Write(stored)
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, errors.Join(fmt.Errorf("writing the file to edit: %w", err), os.Remove(f.Name()))
	}
	return &editing{cmd: cmd, svc: svc, ref: ref, path: f.Name(), stored: stored, text: stored}, nil
}

// run opens the editor until the file holds a valid object, prints the diff, asks and saves the object.
func (e *editing) run(yes bool) error {
	ctx := e.cmd.Context()
	out := e.cmd.OutOrStdout()
	objs, edited, err := e.edit(ctx)
	if err != nil {
		return err
	}
	if edited == nil {
		// After errors, the operator's text may be the only copy of their work.
		if e.header != nil && e.holdsWork() {
			e.keep = true
			return printLine(out, "edit cancelled; nothing saved; your edit is in "+e.path)
		}
		return printLine(out, "edit cancelled; nothing changed")
	}
	if _, err := io.WriteString(out, unifiedDiff(string(e.stored), string(edited))); err != nil {
		return fmt.Errorf("writing the diff: %w", err)
	}
	if !yes {
		ok, err := confirm(ctx, e.cmd)
		if err != nil {
			return err
		}
		if !ok {
			return printLine(out, "not saved")
		}
	}
	changes, err := e.svc.Save(ctx, e.ref, objs, true)
	e.saved = changes != nil
	return report(e.saved, err, func() error { return printChanges(out, outputTable, changes) })
}

// edit opens the editor until the file holds a valid object, and returns the object and its YAML. While the object
// does not decode or is invalid, it opens the editor again with the errors in a header above the operator's text. It
// returns no YAML when the operator saves the file unchanged or empty.
func (e *editing) edit(ctx context.Context) (spec.Objects, []byte, error) {
	for {
		// The file may hold the operator's work even when the editor failed or tent was interrupted.
		edErr := runEditor(e.cmd, e.path)
		text, err := e.read()
		if err != nil {
			return spec.Objects{}, nil, errors.Join(edErr, err)
		}
		unchanged := bytes.Equal(text, e.text)
		e.text = text
		switch {
		case ctx.Err() != nil:
			return spec.Objects{}, nil, errInterrupted
		case edErr != nil:
			return spec.Objects{}, nil, edErr
		case unchanged:
			return spec.Objects{}, nil, nil
		}
		objs, problems := decodeBelowHeader(text)
		if problems == nil {
			edited, err := e.check(ctx, objs)
			if _, invalid := errors.AsType[v1alpha1.Errors](err); !invalid {
				return objs, edited, err
			}
			problems = err
		}
		e.header = errorHeader(problems)
		if err := os.WriteFile(e.path, slices.Concat(e.header, text), 0o600); err != nil {
			return spec.Objects{}, nil, fmt.Errorf("writing the errors to the edited file: %w", err)
		}
	}
}

// read returns the operator's text: the file with LF line ends, without a byte order mark and the header. An editor
// that writes CR LF or a byte order mark writes them before the header too.
func (e *editing) read() ([]byte, error) {
	data, err := os.ReadFile(e.path)
	if err != nil {
		return nil, fmt.Errorf("reading the edited file: %w", err)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	return bytes.TrimPrefix(data, e.header), nil
}

// fixErrors starts the header that shows the operator the errors of an edit.
const fixErrors = "# Please fix the errors below and save, or save it unchanged to cancel; your edit is kept.\n"

// errorHeader returns the header that shows err above the operator's text, as YAML comments.
func errorHeader(err error) []byte {
	var b strings.Builder
	b.WriteString(fixErrors)
	for line := range strings.SplitSeq(strings.TrimSuffix(err.Error(), "\n"), "\n") {
		b.WriteString("# " + line + "\n")
	}
	return []byte(b.String())
}

// decodeBelowHeader decodes the operator's text. Error line numbers count the header above the text; the header has
// the same number of lines whatever numbers it shows.
func decodeBelowHeader(text []byte) (spec.Objects, error) {
	objs, err := spec.Decode(text)
	if err != nil {
		if _, below := spec.Decode(slices.Concat(errorHeader(err), text)); below != nil {
			err = below
		}
	}
	return objs, err
}

// check returns the YAML of objs, the edited object, once Save has checked it. It returns no YAML when objs is empty
// or the same as the stored object.
func (e *editing) check(ctx context.Context, objs spec.Objects) ([]byte, error) {
	if objs.Cluster == nil && len(objs.NodeGroups) == 0 {
		return nil, nil
	}
	edited, err := spec.Encode(objs)
	if err != nil || bytes.Equal(edited, e.stored) {
		return nil, err
	}
	if _, err := e.svc.Save(ctx, e.ref, objs, false); err != nil {
		return nil, err
	}
	return edited, nil
}

// finish removes the file and returns err. It keeps the file when edit says so, and when err stopped the edit before
// it saved the operator's changes: then it adds to err where the file is.
func (e *editing) finish(err error) error {
	switch {
	case e.keep:
		return err
	case err != nil && e.holdsWork():
		return errors.Join(err, errors.New("your edit is in "+e.path))
	}
	if rmErr := os.Remove(e.path); rmErr != nil {
		// A notice that fails to print changes nothing.
		_, _ = fmt.Fprintf(e.cmd.ErrOrStderr(), "WARNING: could not remove the edited file: %v\n", rmErr)
	}
	return err
}

// holdsWork reports whether the file holds the operator's work, unsaved: text that is not blank and not the stored
// object.
func (e *editing) holdsWork() bool {
	return !e.saved && len(bytes.TrimSpace(e.text)) > 0 && !bytes.Equal(e.text, e.stored)
}

// printLine writes line and a line end to w.
func printLine(w io.Writer, line string) error {
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return nil
}

// errInterrupted ends an edit that the operator interrupted.
var errInterrupted = errors.New("interrupted; nothing saved")

// confirm asks on stderr whether to save, and reads the answer from stdin: y or yes, in any case, saves. It stops
// waiting when ctx ends; the read of stdin goes on until tent exits.
func confirm(ctx context.Context, cmd *cobra.Command) (bool, error) {
	stderr := cmd.ErrOrStderr()
	_, _ = io.WriteString(stderr, "Save? [y/N] ") // a question that fails to print can still be answered
	type reply struct {
		answer string
		err    error
	}
	replies := make(chan reply, 1)
	go func() {
		answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		replies <- reply{answer, err}
	}()
	var r reply
	select {
	case <-ctx.Done():
		_, _ = io.WriteString(stderr, "\n") // the error goes on a line of its own
		return false, errInterrupted
	case r = <-replies:
	}
	if r.err != nil && !errors.Is(r.err, io.EOF) {
		return false, fmt.Errorf("reading the answer: %w", r.err)
	}
	switch strings.ToLower(strings.TrimSpace(r.answer)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// runEditor opens the file at path in the editor, on the streams of cmd, and waits for the editor to exit. Ctrl-C
// meanwhile belongs to the editor, and tent never kills it: that could lose the operator's work.
func runEditor(cmd *cobra.Command, path string) error {
	args, err := splitCommand(editorCommand())
	if err != nil {
		return err
	}
	ed := exec.Command(args[0], append(args[1:], path)...)
	ed.Stdin, ed.Stdout, ed.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := whileEditing(cmd.Context(), ed.Run); err != nil {
		return fmt.Errorf("the editor failed: %w", err)
	}
	return nil
}

// Environment variables that name the editor, in the order edit tries them.
var editorEnvs = []string{"TENT_EDITOR", "VISUAL", "EDITOR"}

// editorCommand returns the command line of the editor: the first of TENT_EDITOR, VISUAL and EDITOR that is set, else
// vi, or notepad on Windows.
func editorCommand() string {
	for _, env := range editorEnvs {
		if line := strings.TrimSpace(os.Getenv(env)); line != "" {
			return line
		}
	}
	if runtime.GOOS == "windows" {
		return "notepad"
	}
	return "vi"
}

// splitCommand splits a command line on spaces. Double quotes keep the spaces of what they enclose, as in
// "C:\Program Files\Editor\editor.exe" --wait.
func splitCommand(line string) ([]string, error) {
	var (
		args   []string
		arg    strings.Builder
		inArg  bool // arg has begun, perhaps as ""
		quoted bool
	)
	for _, r := range line {
		switch {
		case r == '"':
			quoted, inArg = !quoted, true
		case unicode.IsSpace(r) && !quoted:
			if inArg {
				args = append(args, arg.String())
				arg.Reset()
				inArg = false
			}
		default:
			arg.WriteRune(r)
			inArg = true
		}
	}
	if quoted {
		return nil, errors.New("the editor command has an unterminated quote: " + line)
	}
	if inArg {
		args = append(args, arg.String())
	}
	return args, nil
}
