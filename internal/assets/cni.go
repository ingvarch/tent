package assets

import (
	"fmt"

	"github.com/ingvarch/tent/internal/channels"
)

// CNI returns the CNI plugins that the channel pins for linux on arch, with the sha256 the channel holds. CNI
// releases carry no signature, so the sha256 is the one checked when the channel was written.
func CNI(ch *channels.Channel, arch string) (Asset, error) {
	sum, ok := ch.CNI.SHA256[arch]
	if !ok {
		return Asset{}, fmt.Errorf("channel %s has no CNI plugins for %s", ch.Name, arch)
	}
	v := ch.CNI.Version
	file := fmt.Sprintf("%s/v%s/cni-plugins-linux-%s-v%s.tgz", cniReleases, v, arch, v)
	return Asset{Name: "cni-plugins", Version: v, URLs: []string{file}, SHA256: sum}, nil
}
