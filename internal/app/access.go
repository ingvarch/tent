package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/nomadops"
	"github.com/ingvarch/tent/internal/pki"
	"github.com/ingvarch/tent/internal/secret"
	"github.com/ingvarch/tent/internal/statestore"
)

// Access is what an operator needs to call the Nomad API of a cluster: a certificate and a token that both end soon.
// Printing it shows the sizes of its key and token, never their content.
type Access struct {
	Cluster  string
	Region   string        // the Nomad region
	Servers  []string      // the API addresses of the servers, as host:port; the one that made the token comes first
	CA       []byte        // the cluster's CA bundle, PEM, which is public
	Cert     []byte        // the operator's certificate, PEM
	Key      secret.Secret // its private key, PEM
	Token    secret.Secret // the secret of a management token, which Nomad stops accepting when the token's TTL is over
	Accessor string        // the token's public id
	Until    time.Time     // the earlier of the token's end and the certificate's end, in UTC
}

// OperatorAccess makes an operator's access to the Nomad API of the cluster for purpose, such as export nomad, that
// ends after ttl: a management token that Nomad makes with the name "tent <purpose> <owner>@<host>", and a certificate
// of the cluster's CA for client authentication only, with a new key. Nomad keeps the token; tent stores neither the
// token nor the certificate.
//
// It reads the state store, which must hold the cluster's secrets and the mark of the bootstrap, and the cloud's list
// of the machines, and it calls Nomad once, through the servers that have a public address. It writes nothing and
// takes no lock. A call that fails or loses its answer may leave a token in Nomad that nobody holds; it ends with its
// TTL. Nomad refuses a TTL of less than a minute or more than a day, unless its servers are set otherwise.
func (s *Service) OperatorAccess(ctx context.Context, cluster, purpose string, ttl time.Duration,
) (_ Access, err error) {
	defer func() { err = stopped(ctx, err) }()
	if ttl <= 0 {
		return Access{}, fmt.Errorf("operator access: TTL %s is not above zero", ttl)
	}
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return Access{}, err
	}
	objs, err := s.get(ctx, l, true)
	if err != nil {
		return Access{}, err
	}
	secrets, err := s.storedSecrets(ctx, l)
	if _, missing := errors.AsType[*missingSecretsError](err); missing {
		return Access{}, fmt.Errorf("%s: %w; run tent update cluster --yes first", clusterLabel(cluster), err)
	}
	if err != nil {
		return Access{}, err
	}
	marked, err := s.bootstrapped(ctx, l)
	if err != nil {
		return Access{}, err
	}
	if !marked {
		return Access{}, fmt.Errorf("%s: Nomad is not bootstrapped yet (%s is missing); run tent update cluster --yes first",
			clusterLabel(cluster), l.NomadBootstrapped())
	}
	p, err := s.provider(objs.Cluster.Spec.Cloud.Provider)
	if err != nil {
		return Access{}, err
	}
	instances, err := p.Nodes().List(ctx, cluster)
	if err != nil {
		return Access{}, err
	}
	machines := slices.DeleteFunc(slices.Clone(instances), func(in cloud.Instance) bool { return !in.Role.RunsServer() })
	if len(machines) == 0 {
		return Access{}, fmt.Errorf("%s has no server machine; run tent update cluster --yes first", clusterLabel(cluster))
	}
	slices.SortFunc(machines, compareName)
	region := objs.Cluster.Spec.Nomad.Region
	servers, err := s.nomadOver(machines, nomadAccess{cluster: cluster, region: region, secrets: secrets})
	if err != nil {
		return Access{}, err
	}
	now := s.now()
	cert, err := secrets.ca.IssueOperator(region, ttl, now)
	if err != nil {
		return Access{}, err
	}
	owner, host := statestore.LocalHolder()
	token, err := servers.CreateToken(ctx, nomadops.TokenRequest{
		Name: fmt.Sprintf("tent %s %s@%s", purpose, owner, host), TTL: ttl,
	})
	if err != nil {
		err = fmt.Errorf("create the operator token: %w", err)
		if errors.Is(err, nomadops.ErrNotReady) {
			err = withReachHint(err)
		}
		return Access{}, err
	}
	return Access{
		Cluster: cluster, Region: region, Servers: answeringFirst(machines, servers.Last()), CA: secrets.ca.Bundle(),
		Cert: cert.Cert, Key: cert.Key, Token: token.Secret, Accessor: token.Accessor,
		Until: earlier(token.Expires, certificateEnd(secrets.ca, now, ttl)).UTC(),
	}, nil
}

// answeringFirst returns the API addresses of the machines that have a public address: first, then the others in the
// order of machines.
func answeringFirst(machines []cloud.Instance, first string) []string {
	var addrs []string
	for _, in := range machines {
		if in.PublicIP.IsValid() && apiAddress(in) != first {
			addrs = append(addrs, apiAddress(in))
		}
	}
	return slices.Insert(addrs, 0, first)
}

// certificateEnd returns when an operator certificate that the CA ca issued at now for ttl ends: ttl after now, or
// when the CA ends if that is sooner, in whole seconds, as a certificate holds its end.
func certificateEnd(ca *pki.CA, now time.Time, ttl time.Duration) time.Time {
	return earlier(now.Add(ttl), ca.Certificate().NotAfter).Truncate(time.Second)
}

// earlier returns the earlier of two times.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
