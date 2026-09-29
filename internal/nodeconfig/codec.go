package nodeconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// wire is the JSON form of a NodeConfig. Its Assets and Files fields hide the embedded ones, so that the assets carry
// their URLs and the files their content, which the JSON forms of Asset and File leave out.
type wire struct {
	NodeConfig
	Assets []wireAsset `json:"assets,omitempty"`
	Files  []wireFile  `json:"files,omitempty"`
}

// wireAsset is the JSON form of an Asset: its fields as they are.
type wireAsset Asset

// wireFile is the JSON form of a File. The content is a string, which reads and compresses better than base64.
type wireFile struct {
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner"`
	PerNode bool   `json:"perNode,omitempty"`
	Secret  bool   `json:"secret,omitempty"`
	Content string `json:"content"`
}

// Encode returns the JSON that tent-node reads: the NodeConfig with the content of its files, indented, with a line
// end. The same NodeConfig always gives the same bytes, and empty lists and maps are left out. It validates nc first.
func Encode(nc *NodeConfig) ([]byte, error) {
	if err := nc.Validate(); err != nil {
		return nil, err
	}
	w := wire{NodeConfig: *nc, Assets: make([]wireAsset, len(nc.Assets)), Files: make([]wireFile, len(nc.Files))}
	for i, a := range nc.Assets {
		w.Assets[i] = wireAsset(a)
	}
	for i, f := range nc.Files {
		w.Files[i] = wireFile{
			Path: f.Path, Mode: f.Mode, Owner: f.Owner, PerNode: f.PerNode, Secret: f.Secret, Content: string(f.Content),
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(w); err != nil {
		return nil, fmt.Errorf("encode node config: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode reads the JSON that Encode writes and validates it. A field it does not know, or anything after the object,
// is an error. Keys match their fields as encoding/json matches them: ignoring case, and a repeated key wins.
func Decode(data []byte) (*NodeConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("decode node config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode node config: data after the object")
	}
	nc := w.NodeConfig
	for _, a := range w.Assets {
		nc.Assets = append(nc.Assets, Asset(a))
	}
	for _, f := range w.Files {
		nc.Files = append(nc.Files, File{
			Path: f.Path, Mode: f.Mode, Owner: f.Owner, Content: []byte(f.Content), PerNode: f.PerNode, Secret: f.Secret,
		})
	}
	if err := nc.Validate(); err != nil {
		return nil, err
	}
	return &nc, nil
}
