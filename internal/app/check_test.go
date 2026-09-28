package app_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ingvarch/tent/api/v1alpha1"
	"github.com/ingvarch/tent/internal/app"
)

func TestCheck(t *testing.T) {
	objs := decode(t, clusterYAML, serversYAML, workersYAML)
	if err := app.Check(objs, v1alpha1.ValidateOptions{}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if diff := cmp.Diff(decode(t, clusterYAML, serversYAML, workersYAML), objs); diff != "" {
		t.Errorf("Check changed its input (-want +got):\n%s", diff)
	}

	single := decode(t, clusterYAML, edit(t, serversYAML, "size: 3", "size: 1"))
	wantFieldErrors(t, app.Check(single, v1alpha1.ValidateOptions{}), v1alpha1.FieldError{
		Object: "NodeGroup servers", Path: "spec.size", Detail: "size 1 needs --allow-single-server",
	})
	if err := app.Check(single, v1alpha1.ValidateOptions{AllowSingleServer: true}); err != nil {
		t.Errorf("Check with AllowSingleServer: %v", err)
	}
}

// TestCheckChecksTheChannel checks the channel and the Nomad version against the channels embedded in tent.
func TestCheckChecksTheChannel(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		want      v1alpha1.FieldError
	}{
		{"unknown channel", edit(t, clusterYAML, "region: ams", "region: ams\n  channel: x"),
			problem("spec.channel", `unknown channel "x"; known: stable`)},
		{"version older than the channel's minimum", withVersion(t, clusterYAML, "1.11.0"),
			problem("spec.nomad.version", "1.11.0 is older than 2.0.0, the oldest Nomad that channel stable allows")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantFieldErrors(t, app.Check(decode(t, tc.doc, serversYAML, workersYAML), v1alpha1.ValidateOptions{}),
				tc.want)
		})
	}
}
