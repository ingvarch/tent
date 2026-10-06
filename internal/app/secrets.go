package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ingvarch/tent/internal/english"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/statestore"
)

// secretWrite is an object of a cluster's secrets that an update writes, with its content.
type secretWrite struct {
	path string
	data pki.Secret
}

// clusterSecrets are the secrets a run uses: the stored ones, or the new ones that writes holds.
type clusterSecrets struct {
	ca        *pki.CA
	gossip    pki.Secret
	bootstrap pki.Secret
	writes    []secretWrite // the secrets that the store lacks, with their new contents
}

// planSecrets reads which of the cluster's secrets the store holds: the CA's key and bundle, the gossip key and the
// ACL bootstrap secret. It returns all four, the stored values or new ones, and in writes the missing ones with their
// new contents, in the order the layout gives: a new CA's key and bundle, or a bundle signed with the stored key when
// the key is there alone; a new gossip key; a new bootstrap secret. A bundle without its key, or a stored secret that
// does not load, is an error that names the paths: tent never replaces a cluster's secrets.
func (s *Service) planSecrets(ctx context.Context, l statestore.Layout) (clusterSecrets, error) {
	stored := map[string]pki.Secret{}
	for _, p := range l.Secrets() {
		data, _, err := s.Store.Get(ctx, p)
		switch {
		case errors.Is(err, statestore.ErrNotFound):
		case err != nil:
			return clusterSecrets{}, err
		default:
			stored[p] = data
		}
	}
	made, current := map[string]pki.Secret{}, map[string]pki.Secret{}
	ca, err := s.planCA(l, stored, made)
	if err != nil {
		return clusterSecrets{}, err
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
			data = sec.make()
			made[sec.path] = data
		} else if err := sec.check(data); err != nil {
			return clusterSecrets{}, fmt.Errorf("%s: %w", sec.path, err)
		}
		current[sec.path] = data
	}
	secrets := clusterSecrets{ca: ca, gossip: current[l.GossipKey()], bootstrap: current[l.ACLBootstrapSecret()]}
	for _, p := range l.Secrets() {
		if data, ok := made[p]; ok {
			secrets.writes = append(secrets.writes, secretWrite{p, data})
		}
	}
	return secrets, nil
}

// planCA returns the cluster's CA: the stored one, or a new one. It checks the stored key and bundle, and puts the
// contents that complete the CA into made, by path, as planSecrets says.
func (s *Service) planCA(l statestore.Layout, stored, made map[string]pki.Secret) (*pki.CA, error) {
	key, hasKey := stored[l.CAKey()]
	bundle, hasBundle := stored[l.CABundle()]
	switch {
	case !hasKey && !hasBundle:
		ca, err := pki.NewCA(l.Cluster(), s.now())
		if err != nil {
			return nil, err
		}
		made[l.CAKey()], made[l.CABundle()] = ca.Key(), pki.Secret(ca.Bundle())
		return ca, nil
	case !hasKey:
		return nil, fmt.Errorf("the CA key %s is missing, but the CA bundle %s is there; restore the key: tent never "+
			"replaces a cluster's CA", l.CAKey(), l.CABundle())
	case !hasBundle:
		ca, err := pki.NewCAFromKey(l.Cluster(), key, s.now())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.CAKey(), err)
		}
		made[l.CABundle()] = pki.Secret(ca.Bundle())
		return ca, nil
	}
	ca, err := pki.LoadCA(bundle, key)
	if err != nil {
		return nil, fmt.Errorf("%s and %s: %w", l.CAKey(), l.CABundle(), err)
	}
	return ca, nil
}

// relativePaths returns the paths of the writes relative to the cluster of the layout l, such as pki/private/ca.key.
func relativePaths(l statestore.Layout, writes []secretWrite) []string {
	var paths []string
	for _, w := range writes {
		paths = append(paths, strings.TrimPrefix(w.path, l.Prefix()))
	}
	return paths
}

// missingSecretsError says which of a cluster's secrets the store lacks.
type missingSecretsError struct{ paths []string }

func (e *missingSecretsError) Error() string { return "the state store lacks " + english.And(e.paths) }

// storedSecrets returns the secrets of the cluster of the layout l as the store holds them. It writes nothing: when the
// store lacks any secret it fails with a *missingSecretsError that names the paths relative to the cluster, and a
// stored secret that does not load is an error as planSecrets says.
func (s *Service) storedSecrets(ctx context.Context, l statestore.Layout) (clusterSecrets, error) {
	secrets, err := s.planSecrets(ctx, l)
	if err != nil {
		return clusterSecrets{}, err
	}
	if len(secrets.writes) > 0 {
		return clusterSecrets{}, &missingSecretsError{paths: relativePaths(l, secrets.writes)}
	}
	return secrets, nil
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
