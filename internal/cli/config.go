package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// fileConfig holds the values of the config file. An empty value is unset.
type fileConfig struct {
	state, cluster, output, logFormat string
}

// configPath returns where the config file is: $XDG_CONFIG_HOME/tent/config.yaml, else ~/.config/tent/config.yaml on
// every operating system. The error says why there is no home directory.
func configPath() (string, error) {
	dir, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// cachePath returns tent's cache directory: $XDG_CACHE_HOME/tent, else ~/.cache/tent on every operating system. The
// error says why there is no home directory.
func cachePath() (string, error) { return xdgDir("XDG_CACHE_HOME", ".cache") }

// xdgDir returns tent's directory in the XDG directory that the variable env names, else in fallback under the home
// directory.
func xdgDir(env, fallback string) (string, error) {
	dir := os.Getenv(env)
	if !filepath.IsAbs(dir) { // unset or relative, which the XDG spec says to ignore
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, fallback)
	}
	return filepath.Join(dir, "tent"), nil
}

// readConfig reads the config file at path. A missing file sets nothing.
func readConfig(path string) (fileConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileConfig{}, nil
	}
	if err != nil {
		return fileConfig{}, fmt.Errorf("reading the config file: %w", err)
	}
	c, err := parseConfig(data)
	if err != nil {
		return fileConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// parseConfig decodes a config file: one YAML mapping with the string keys state, cluster, output and logFormat.
// Keys are case-sensitive. Errors name the line.
func parseConfig(data []byte) (fileConfig, error) {
	root, err := configRoot(data)
	if err != nil {
		return fileConfig{}, err
	}
	var c fileConfig
	fields := map[string]struct {
		value *string
		check func(string) error
	}{
		"state":     {&c.state, nil},
		"cluster":   {&c.cluster, nil},
		"output":    {&c.output, validateOutput},
		"logFormat": {&c.logFormat, validateLogFormat},
	}
	seen := make(map[string]bool, len(fields))
	for i := 0; i+1 < len(root.Content); i += 2 {
		line := root.Content[i].Line
		k, v := unalias(root.Content[i]), unalias(root.Content[i+1])
		f, ok := fields[k.Value]
		switch {
		case !ok:
			return fileConfig{}, fmt.Errorf("line %d: unknown key %q: want state, cluster, output or logFormat",
				line, k.Value)
		case seen[k.Value]:
			return fileConfig{}, fmt.Errorf("line %d: duplicate key %q", line, k.Value)
		case v.Kind != yaml.ScalarNode || v.ShortTag() != "!!str":
			return fileConfig{}, fmt.Errorf("line %d: %s: must be a string", line, k.Value)
		}
		if f.check != nil {
			if err := f.check(v.Value); err != nil {
				return fileConfig{}, fmt.Errorf("line %d: %w", line, err)
			}
		}
		seen[k.Value] = true
		*f.value = v.Value
	}
	return c, nil
}

// configRoot returns the mapping of the config file's only document. A file without a document, or with an empty
// one, is an empty mapping.
func configRoot(data []byte) (*yaml.Node, error) {
	empty := &yaml.Node{Kind: yaml.MappingNode}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return empty, nil
	} else if err != nil {
		return nil, err
	}
	var next yaml.Node
	if err := dec.Decode(&next); err == nil {
		return nil, fmt.Errorf("line %d: more than one document", next.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	root := doc.Content[0]
	switch {
	case root.ShortTag() == "!!null":
		return empty, nil
	case root.Kind != yaml.MappingNode:
		return nil, fmt.Errorf("line %d: not a mapping", root.Line)
	}
	return root, nil
}

// unalias returns the node an alias stands for, or n itself.
func unalias(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.AliasNode {
		return n.Alias
	}
	return n
}
