package vultr

import (
	"fmt"
	"slices"
	"strings"
)

// image is an operating system image that tent supports on Vultr.
type image struct {
	name string // the name specs give it, such as ubuntu-24.04
	osID int    // Vultr's id of the image, the os_id of an instance
}

// images are the images tent supports on Vultr, in the order messages list them.
var images = []image{
	{name: "ubuntu-24.04", osID: 2284}, // Ubuntu 24.04 LTS x64
	{name: "ubuntu-26.04", osID: 2760}, // Ubuntu 26.04 LTS x64
}

// osID returns Vultr's os_id of the image that a spec names, such as 2284 for ubuntu-24.04, and false for an image
// that tent does not support on Vultr.
func osID(name string) (int, bool) {
	i := slices.IndexFunc(images, func(im image) bool { return im.name == name })
	if i < 0 {
		return 0, false
	}
	return images[i].osID, true
}

// unsupportedImage says that tent does not support the image name on Vultr, and lists the images it supports.
func unsupportedImage(name string) string {
	return fmt.Sprintf("tent supports %s on Vultr, not %q", imageNames(), name)
}

// imageNames lists the names of the images tent supports on Vultr, such as "ubuntu-24.04 and ubuntu-26.04".
func imageNames() string {
	names := make([]string, len(images))
	for i, im := range images {
		names[i] = im.name
	}
	return andList(names)
}

// andList joins items as a sentence lists them: "a", "a and b", "a, b and c".
func andList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	last := len(items) - 1
	return strings.Join(items[:last], ", ") + " and " + items[last]
}
