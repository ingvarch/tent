package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
	"github.com/ingvarch/tent/internal/shellenv"
)

// exportTTL is how long the access that export nomad makes works, unless --ttl says otherwise.
const exportTTL = 24 * time.Hour

// The files that export nomad writes into its directory.
const (
	exportCAFile    = "ca.pem"
	exportCertFile  = "cli.pem"
	exportKeyFile   = "cli-key.pem"
	exportTokenFile = "token"
)

func newExportCommand(opts *globalOptions) *cobra.Command {
	cmd := groupCommand("export", "Export access to a cluster")
	cmd.AddCommand(newExportNomadCommand(opts))
	return cmd
}

func newExportNomadCommand(opts *globalOptions) *cobra.Command {
	var ttl time.Duration
	var dir, shell string
	cmd := &cobra.Command{
		Use:   "nomad [NAME]",
		Short: "Write the access to a cluster's Nomad API and print the lines that use it",
		Long: "Make an operator's access to the Nomad API of the cluster named by NAME or --name, write it to " +
			"files, and print the shell lines that point the nomad CLI at them. The cluster's CA key and its ACL " +
			"bootstrap secret stay in the state store. The command writes four files into the directory: " +
			"ca.pem, the cluster's CA bundle; cli.pem and cli-key.pem, a client certificate that the cluster's CA " +
			"issued and its private key; and token, the secret of a new management token. The certificate and the token " +
			"work for --ttl, 24 hours by default; Nomad refuses a TTL below 1 minute or above 24 hours unless " +
			"its servers are set otherwise. " +
			"The directory is --dir, else $XDG_CACHE_HOME/tent/NAME, else ~/.cache/tent/NAME. " +
			"tent makes it with mode 0700 when it does not exist, and leaves the mode of one that does. The files " +
			"have mode 0600 either way. A new run replaces the files of the earlier run; the earlier token works " +
			"until it ends. A directory that tent made is removed again when the access cannot be made. " +
			"The command prints six lines on stdout that set NOMAD_ADDR, NOMAD_CACERT, NOMAD_CLIENT_CERT, " +
			"NOMAD_CLIENT_KEY, NOMAD_TLS_SERVER_NAME and NOMAD_TOKEN, for sh or fish: --shell, else fish when " +
			"$SHELL names fish, else sh. In sh, bash or zsh run: eval \"$(tent export nomad NAME)\". In fish " +
			"run: tent export nomad NAME | source. With -o json or -o yaml it prints the paths, the address and " +
			"the end of the access instead, and no shell line. A notice on stderr says where the files are, when " +
			"the access ends and the token's accessor; nomad acl token delete ACCESSOR revokes the token early. " +
			"The token's secret and the key are never printed. The command needs the cloud's credentials in " +
			"the environment, VULTR_API_KEY for Vultr, and a way to port 4646 of the servers, which " +
			"spec.access.api allows. It changes nothing in the state store and takes no lock.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := opts.clusterArg(args)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("shell") {
				shell = shellenv.Login(os.Getenv("SHELL"))
			} else if !shellenv.Valid(shell) {
				return fmt.Errorf("invalid --shell %q: want %s or %s", shell, shellenv.Sh, shellenv.Fish)
			}
			if ttl <= 0 {
				return fmt.Errorf("invalid --ttl %s: must be above zero", ttl)
			}
			if err := v1alpha1.ValidateName(v1alpha1.KindCluster, name); err != nil {
				return err
			}
			dir, err = exportDir(dir, name)
			if err != nil {
				return err
			}
			svc, err := opts.service(cmd, v1alpha1.ValidateOptions{})
			if err != nil {
				return err
			}
			_, statErr := os.Stat(dir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("create the directory %s: %w", dir, err)
			}
			access, err := svc.OperatorAccess(cmd.Context(), name, "export nomad", ttl)
			if err != nil {
				if errors.Is(statErr, fs.ErrNotExist) {
					_ = os.Remove(dir) // this run made it, and only an empty directory goes
				}
				return err
			}
			if err := writeAccess(dir, access); err != nil {
				return err
			}
			exp := newNomadExport(dir, access)
			// A notice that fails to print changes nothing.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote the Nomad access of cluster %s to %s; it works until %s "+
				"(token accessor %s)\n", name, dir, exp.Expires.Format("2006-01-02 15:04:05 MST"), exp.TokenAccessor)
			return printObject(cmd.OutOrStdout(), opts.output, exp, func(w io.Writer) error {
				return exp.writeLines(w, shell)
			})
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", exportTTL, "how long the certificate and the token work")
	cmd.Flags().StringVar(&dir, "dir", "", "the directory for the files (default $XDG_CACHE_HOME/tent/NAME, "+
		"else ~/.cache/tent/NAME)")
	cmd.Flags().StringVar(&shell, "shell", "", "the shell to print lines for: sh or fish (default fish when $SHELL "+
		"names fish, else sh)")
	return cmd
}

// exportDir returns the absolute directory that holds the files of the cluster's access: dir, or tent's cache
// directory of the cluster when dir is empty.
func exportDir(dir, cluster string) (string, error) {
	if dir == "" {
		cache, err := cachePath()
		if err != nil {
			return "", fmt.Errorf("the default directory: %w; give --dir", err)
		}
		dir = filepath.Join(cache, cluster)
	}
	return filepath.Abs(dir)
}

// exportFile is a file that export nomad writes: its final path and its content.
type exportFile struct {
	path string
	data []byte
}

// writeAccess writes the files of access into dir, in the order ca.pem, cli.pem, cli-key.pem and token.
func writeAccess(dir string, access app.Access) error {
	return writeFiles([]exportFile{
		{filepath.Join(dir, exportCAFile), access.CA},
		{filepath.Join(dir, exportCertFile), access.Cert},
		{filepath.Join(dir, exportKeyFile), access.Key.Bytes()},
		{filepath.Join(dir, exportTokenFile), access.Token.Bytes()},
	})
}

// writeFiles gives each file its content and the mode 0600. It writes a temporary file beside each one first, created
// with that mode, and renames them over the final paths in the order given, so a reader sees an old file or a new one
// whole. When a write fails, it removes every temporary file and leaves the files of an earlier run as they were. A
// rename that fails in the middle leaves the files before it replaced and the files after it as they were: a set of
// old and new files. A failure names the final path of its file.
func writeFiles(files []exportFile) error {
	temps := make([]string, 0, len(files))
	removeFrom := func(i int) {
		for _, tmp := range temps[i:] {
			_ = os.Remove(tmp)
		}
	}
	for _, f := range files {
		tmp, err := writeTemp(f.path, f.data)
		if err != nil {
			removeFrom(0)
			return fmt.Errorf("write %s: %w", f.path, err)
		}
		temps = append(temps, tmp)
	}
	for i, f := range files {
		if err := os.Rename(temps[i], f.path); err != nil {
			removeFrom(i)
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}
	return nil
}

// writeTemp writes data into a new file beside path, with the mode 0600, and returns its name. A failure removes the
// file.
func writeTemp(path string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(data)
	if err = errors.Join(err, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// nomadExport says where export nomad put the access, which address it is for and when it ends. It is what -o json
// and -o yaml print.
type nomadExport struct {
	Cluster       string    `json:"cluster"`
	Dir           string    `json:"dir"`
	Address       string    `json:"address"`
	CACert        string    `json:"caCert"`
	ClientCert    string    `json:"clientCert"`
	ClientKey     string    `json:"clientKey"`
	TLSServerName string    `json:"tlsServerName"`
	TokenFile     string    `json:"tokenFile"`
	TokenAccessor string    `json:"tokenAccessor"`
	Expires       time.Time `json:"expires"` // in whole seconds
}

// newNomadExport returns the description of access, whose files are in dir.
func newNomadExport(dir string, access app.Access) nomadExport {
	return nomadExport{
		Cluster:       access.Cluster,
		Dir:           dir,
		Address:       "https://" + access.Servers[0],
		CACert:        filepath.Join(dir, exportCAFile),
		ClientCert:    filepath.Join(dir, exportCertFile),
		ClientKey:     filepath.Join(dir, exportKeyFile),
		TLSServerName: "server." + access.Region + ".nomad",
		TokenFile:     filepath.Join(dir, exportTokenFile),
		TokenAccessor: access.Accessor,
		Expires:       access.Until.Truncate(time.Second),
	}
}

// writeLines writes the lines that set the variables of the nomad CLI in the shell. NOMAD_TOKEN reads the token file
// when the shell runs its line, so no secret is in the lines.
func (e nomadExport) writeLines(w io.Writer, shell string) error {
	var b strings.Builder
	for _, v := range []struct{ name, value string }{
		{"NOMAD_ADDR", e.Address},
		{"NOMAD_CACERT", e.CACert},
		{"NOMAD_CLIENT_CERT", e.ClientCert},
		{"NOMAD_CLIENT_KEY", e.ClientKey},
		{"NOMAD_TLS_SERVER_NAME", e.TLSServerName},
	} {
		b.WriteString(shellenv.ExportLine(shell, v.name, v.value) + "\n")
	}
	b.WriteString(shellenv.FileLine(shell, "NOMAD_TOKEN", e.TokenFile) + "\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("writing the lines: %w", err)
	}
	return nil
}
