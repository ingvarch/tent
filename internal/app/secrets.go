package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/statestore"
)

// secretWrite is an object of a cluster's secrets that an update writes, with its content.
type secretWrite struct {
	path string
	data pki.Secret
}

// planSecrets reads which of the cluster's secrets the store holds: the CA's key and bundle, the gossip key and the
// ACL bootstrap secret. It returns the missing ones with new contents, in the order the layout gives: a new CA's key
// and bundle, or a bundle signed with the stored key when the key is there alone; a new gossip key; a new bootstrap
// secret. A bundle without its key, or a stored secret that does not load, is an error that names the paths: tent
// never replaces a cluster's secrets.
func (s *Service) planSecrets(ctx context.Context, l statestore.Layout) ([]secretWrite, error) {
	stored := map[string]pki.Secret{}
	for _, p := range l.Secrets() {
		data, _, err := s.Store.Get(ctx, p)
		switch {
		case errors.Is(err, statestore.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			stored[p] = data
		}
	}
	made := map[string]pki.Secret{}
	if err := s.planCA(l, stored, made); err != nil {
		return nil, err
	}
	for _, sec := range []struct {
		path  string
		check func(pki.Secret) error
		make  func() pki.Secret
	}{
		{l.GossipKey(), pki.CheckGossipKey, pki.NewGossipKey},
		{l.ACLBootstrapSecret(), pki.CheckBootstrapSecret, pki.NewBootstrapSecret},
	} {
		data, ok := stored[sec.path]
		if !ok {
			made[sec.path] = sec.make()
			continue
		}
		if err := sec.check(data); err != nil {
			return nil, fmt.Errorf("%s: %w", sec.path, err)
		}
	}
	var writes []secretWrite
	for _, p := range l.Secrets() {
		if data, ok := made[p]; ok {
			writes = append(writes, secretWrite{p, data})
		}
	}
	return writes, nil
}

// planCA checks the cluster's CA among the stored secrets and puts the contents that complete it into made, by path,
// as planSecrets says.
func (s *Service) planCA(l statestore.Layout, stored, made map[string]pki.Secret) error {
	key, hasKey := stored[l.CAKey()]
	bundle, hasBundle := stored[l.CABundle()]
	switch {
	case !hasKey && !hasBundle:
		ca, err := pki.NewCA(l.Cluster(), s.now())
		if err != nil {
			return err
		}
		made[l.CAKey()], made[l.CABundle()] = ca.Key(), pki.Secret(ca.Bundle())
	case !hasKey:
		return fmt.Errorf("the CA key %s is missing, but the CA bundle %s is there; restore the key: tent never "+
			"replaces a cluster's CA", l.CAKey(), l.CABundle())
	case !hasBundle:
		ca, err := pki.NewCAFromKey(l.Cluster(), key, s.now())
		if err != nil {
			return fmt.Errorf("%s: %w", l.CAKey(), err)
		}
		made[l.CABundle()] = pki.Secret(ca.Bundle())
	default:
		if _, err := pki.LoadCA(bundle, key); err != nil {
			return fmt.Errorf("%s and %s: %w", l.CAKey(), l.CABundle(), err)
		}
	}
	return nil
}

// relativePaths returns the paths of the writes relative to the cluster of the layout l, such as pki/private/ca.key.
func relativePaths(l statestore.Layout, writes []secretWrite) []string {
	var paths []string
	for _, w := range writes {
		paths = append(paths, strings.TrimPrefix(w.path, l.Prefix()))
	}
	return paths
}

// writeSecrets writes the secrets in order. On a store that can create an object only when it does not exist yet,
// each write does so, and a secret that another writer stored meanwhile stops the writes and stays.
func (s *Service) writeSecrets(ctx context.Context, writes []secretWrite) error {
	if len(writes) == 0 {
		return nil
	}
	caps, err := s.Store.Capabilities(ctx)
	if err != nil {
		return err
	}
	opts := statestore.PutOptions{IfNoneMatch: caps.ConditionalPut}
	for _, w := range writes {
		_, err := s.Store.Put(ctx, w.path, w.data.Bytes(), opts)
		switch {
		case errors.Is(err, statestore.ErrPreconditionFailed):
			return errors.New(w.path + " was written meanwhile; run the command again")
		case err != nil:
			return fmt.Errorf("write the secrets: %w", err)
		}
	}
	return nil
}
