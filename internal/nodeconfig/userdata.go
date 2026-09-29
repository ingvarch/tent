package nodeconfig

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

// MaxUserDataBytes is the most user data that UserData returns, 24 KiB, which leaves room under the smallest limit
// of the providers tent supports.
const MaxUserDataBytes = 24 << 10

// TentNodeAsset is the name of the asset that holds the tent-node binary.
const TentNodeAsset = "tent-node"

// ConfigPath is where the user data writes the NodeConfig, and where tent-node reads it.
const ConfigPath = "/etc/tent/node.json"

// tentNodeDownload is where the script puts tent-node until its sha256 is checked.
const tentNodeDownload = "/usr/local/bin/tent-node.download"

// userDataView is what the template of the user data shows.
type userDataView struct {
	Payload string // node.json, compressed with gzip and encoded with base64
	URLs    string // the tent-node URLs, each one shell word
	Check   string // the line that sha256sum -c reads, one shell word
}

// UserData returns the cloud-config that turns a fresh machine into the node: it writes the NodeConfig to
// /etc/tent/node.json, which only root reads, as gzip and base64; downloads tent-node from the first of the tent-node
// asset's URLs that gives a file with the asset's sha256; and runs tent-node install. It turns package updates off:
// nodes are patched by replacing them.
//
// It validates nc first. The config needs an asset named tent-node whose URLs are printable ASCII, and the user data
// must fit in MaxUserDataBytes; the error then names the node group and the size.
func UserData(nc *NodeConfig) ([]byte, error) {
	data, err := userData(nc)
	if err != nil {
		return nil, fmt.Errorf("user data: %w", err)
	}
	return data, nil
}

func userData(nc *NodeConfig) ([]byte, error) {
	node, err := Encode(nc)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(nc.Assets, func(a Asset) bool { return a.Name == TentNodeAsset })
	if i < 0 {
		return nil, errors.New("no tent-node asset")
	}
	tentNode := nc.Assets[i]
	words := make([]string, len(tentNode.URLs))
	for j, u := range tentNode.URLs {
		// Printable ASCII in single quotes stays one inert word both in the YAML block and in sh. The error leaves the
		// URL out: a presigned URL carries a signature.
		if strings.ContainsFunc(u, func(r rune) bool { return r < '!' || r > '~' }) {
			return nil, fmt.Errorf("tent-node URLs[%d] has a character that is not printable ASCII", j)
		}
		words[j] = shellQuote(u)
	}
	v := userDataView{
		Payload: gzipBase64(node),
		URLs:    strings.Join(words, " "),
		Check:   shellQuote(tentNode.SHA256 + "  " + tentNodeDownload),
	}
	var buf bytes.Buffer
	if err := userDataTemplate.Execute(&buf, v); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}
	if buf.Len() > MaxUserDataBytes {
		return nil, fmt.Errorf("node group %s needs %d bytes, more than the %d that fit", nc.NodeGroup, buf.Len(),
			MaxUserDataBytes)
	}
	return buf.Bytes(), nil
}

// gzipBase64 compresses data with gzip, as tightly as it can and without a name or a time, so the same data always
// gives the same text, and encodes it with standard base64.
func gzipBase64(data []byte) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // never fails: the level is valid
	_, _ = zw.Write(data)                                    // never fails: a bytes.Buffer takes every write
	_ = zw.Close()                                           // never fails, as Write
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// shellQuote returns s as one sh word that stands for s itself: in single quotes, where a single quote of s ends the
// quoted part, follows escaped by a backslash and starts the next quoted part.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// userDataTemplate renders the cloud-config from a userDataView. The script tries each URL in turn and installs the
// first download whose sha256 matches.
//   - curl counts a timeout as a transient error and retries it, so a timeout alone would hold a stalled mirror for
//     all its tries. A transfer slower than 1 KiB/s for 30 s fails, and a try fails after 10 minutes. No new try of
//     one URL starts after 10 minutes, so one URL takes at most about 20, and the next mirror gets its turn.
//   - curl retries every error, a refused connection too, as when the network is not up yet at boot. -o writes each
//     try afresh, so a failed try leaves nothing behind.
//   - mkdir makes /usr/local/bin with the usual mode 0755 where an image lacks it: curl's --create-dirs would make it
//     0750.
var userDataTemplate = template.Must(template.New("user-data").Parse(`#cloud-config
package_update: false
package_upgrade: false
write_files:
  - path: ` + ConfigPath + `
    encoding: gz+b64
    owner: root:root
    permissions: "0600"
    content: {{.Payload}}
runcmd:
  - - /bin/sh
    - -c
    - |
      set -eu
      mkdir -p /usr/local/bin
      for url in {{.URLs}}; do
        if curl -fsSL --connect-timeout 10 --speed-limit 1024 --speed-time 30 --max-time 600 --retry 5 \
          --retry-all-errors --retry-max-time 600 -o ` + tentNodeDownload + ` "$url" &&
          echo {{.Check}} | sha256sum -c -; then
          chmod 0755 ` + tentNodeDownload + `
          mv ` + tentNodeDownload + ` /usr/local/bin/tent-node
          exec /usr/local/bin/tent-node install --config ` + ConfigPath + `
        fi
      done
      echo "tent-node: no URL gave a file with the expected sha256" >&2
      exit 1
`))
