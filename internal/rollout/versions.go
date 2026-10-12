package rollout

import "golang.org/x/mod/semver"

// CheckNewNode says whether a new node may run the Nomad version beside the servers and the nodes that n reports.
// It returns an error that matches ErrRefused when version is no version number; when a server, or a node that is
// not down, reports a version that is no version number or a newer Nomad; and, with clientOnly, when a server
// reports an older Nomad: a node that runs no server must not run a newer Nomad than a server. A server that
// reports no version, as one does for a moment after it joins, is not compared, and unknown tells that there is
// one.
func CheckNewNode(n Nomad, version string, clientOnly bool) (unknown bool, err error) {
	if err := checkVersion("a new node", version); err != nil {
		return false, err
	}
	for _, srv := range n.Servers {
		if srv.Version == "" {
			unknown = true
			continue
		}
		if err := checkNotNewer(version, "server "+srv.Name, srv.Version); err != nil {
			return false, err
		}
	}
	for _, node := range n.Nodes {
		if node.Status == nodeDown {
			continue
		}
		if err := checkNotNewer(version, "node "+node.Name, node.Version); err != nil {
			return false, err
		}
	}
	if clientOnly {
		for _, srv := range n.Servers {
			if srv.Version != "" && semver.Compare("v"+srv.Version, "v"+version) < 0 {
				return false, refuse("a new node would run Nomad %s, newer than the %s of server %s; "+
					"roll the servers first", version, srv.Version, srv.Name)
			}
		}
	}
	return unknown, nil
}

func checkVersion(owner, version string) error {
	if !semver.IsValid("v" + version) {
		return refuse("the Nomad version of %s is %q, which is not a version number", owner, version)
	}
	return nil
}

func checkNotNewer(version, owner, reported string) error {
	if err := checkVersion(owner, reported); err != nil {
		return err
	}
	if semver.Compare("v"+reported, "v"+version) > 0 {
		return refuse("tent never moves a node to an older Nomad: the cluster's version is %s, and %s runs %s",
			version, owner, reported)
	}
	return nil
}
