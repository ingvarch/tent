package v1alpha1

// testSSHKey is a valid ed25519 public key for the example cluster.
const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILVMgcq7nf63leSBwZNfB40Oi4XwSKWNKchNmRGNCb9k ops@example"

// cluster returns the Vultr cluster of TestClusterJSONRoundTrip with every defaulted field left empty.
func cluster() *Cluster {
	return &Cluster{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindCluster},
		Metadata: ClusterMeta{Name: "prod"},
		Spec: ClusterSpec{
			Cloud: Cloud{
				Provider: ProviderVultr,
				Region:   "ams",
				Vultr:    &VultrCloud{},
			},
			Access: Access{
				SSH: []string{"203.0.113.7/32"},
			},
			SSHKeys: []string{testSSHKey},
			Nomad: ClusterNomad{
				Version: "2.0.7",
			},
		},
	}
}

// groups returns the servers and workers groups of the cluster with every defaulted field left empty.
func groups() []*NodeGroup {
	return []*NodeGroup{
		{
			TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindNodeGroup},
			Metadata: NodeGroupMeta{Name: "servers", Cluster: "prod"},
			Spec: NodeGroupSpec{
				Role:        RoleServer,
				MachineType: "vc2-2c-4gb",
				Size:        3,
			},
		},
		{
			TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: KindNodeGroup},
			Metadata: NodeGroupMeta{Name: "workers", Cluster: "prod"},
			Spec: NodeGroupSpec{
				Role:        RoleClient,
				MachineType: "vc2-2c-4gb",
				Size:        3,
				Nomad: NodeGroupNomad{
					NodeClass: "general",
					Drivers:   []string{"docker", "exec"},
					Meta:      map[string]string{"team": "platform"},
				},
			},
		},
	}
}

// objects is a cluster with its node groups, the input of SetDefaults and Validate.
type objects struct {
	Cluster    *Cluster
	NodeGroups []*NodeGroup
}

// fixtures returns the Vultr cluster and its groups with every defaulted field left empty.
func fixtures() objects { return objects{Cluster: cluster(), NodeGroups: groups()} }

// hetzner returns the fixtures moved to Hetzner, with three zones.
func hetzner() objects {
	o := fixtures()
	o.Cluster.Spec.Cloud = Cloud{
		Provider: ProviderHetzner,
		Region:   "eu-central",
		Zones:    []string{"fsn1", "nbg1", "hel1"},
		Hetzner:  &HetznerCloud{},
	}
	return o
}

// combined returns the fixtures with the servers group turned into a combined group.
func combined() objects {
	o := fixtures()
	o.NodeGroups[0].Spec.Role = RoleCombined
	return o
}
