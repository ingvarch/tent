package vultr_test

import (
	"strconv"
	"testing"

	"github.com/ingvarch/tent/internal/cloud"
	"github.com/ingvarch/tent/internal/cloud/vultr"
	"github.com/ingvarch/tent/internal/nodeconfig"
)

// TestTagsTakeSpecHash checks that the spec hashes of NodeConfig pass the tag codec both ways as the spec-hash label.
func TestTagsTakeSpecHash(t *testing.T) {
	for i := range 100 {
		nc := &nodeconfig.NodeConfig{
			System: nodeconfig.System{Sysctls: map[string]string{"vm.max_map_count": strconv.Itoa(i)}},
		}
		hash := nodeconfig.SpecHash(nc)
		tags, err := vultr.EncodeTags(cloud.Labels{cloud.LabelSpecHash: hash})
		if err != nil {
			t.Fatalf("EncodeTags(spec hash %s): %v", hash, err)
		}
		got, err := vultr.DecodeTags(tags)
		if err != nil || got[cloud.LabelSpecHash] != hash {
			t.Fatalf("DecodeTags(%q) = %v, %v; want the spec hash %s", tags, got, err, hash)
		}
	}
}
