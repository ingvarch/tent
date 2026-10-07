package e2e

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// settings is what the environment tells the suite. The key never appears in its text.
type settings struct {
	Key     string
	NodeURL string
	NodeSHA string
	// Tent is the path of the tent binary that make build wrote next to the tent-node at NodeURL.
	Tent   string
	Region string
	Plan   string
	Runner string
	Images []string
	Keep   bool
}

// String describes the run without the key, the node URL or its digest.
func (s settings) String() string {
	return fmt.Sprintf("region %s, plan %s, images %s, keep %t", s.Region, s.Plan, strings.Join(s.Images, ","), s.Keep)
}

var imageForm = regexp.MustCompile(`^ubuntu-\d\d\.\d\d$`)

// makeE2EHint ends the error about missing variables.
const makeE2EHint = "; make e2e sets all but VULTR_API_KEY"

// readSettings reads the suite's environment through getenv. It names every missing required variable in one error.
func readSettings(getenv func(string) string) (settings, error) {
	s := settings{
		Key:     getenv("VULTR_API_KEY"),
		NodeURL: getenv("TENT_NODE_URL"),
		NodeSHA: getenv("TENT_NODE_SHA256"),
		Tent:    getenv("E2E_TENT"),
		Region:  valueOr(getenv("E2E_REGION"), "ams"),
		Plan:    valueOr(getenv("E2E_PLAN"), "vc2-1c-1gb"),
		Runner:  getenv("RUNNER_ADDR"),
		Keep:    getenv("E2E_KEEP") != "",
	}
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"VULTR_API_KEY", s.Key}, {"TENT_NODE_URL", s.NodeURL}, {"TENT_NODE_SHA256", s.NodeSHA}, {"E2E_TENT", s.Tent},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return settings{}, errors.New("set " + joinWithAnd(missing) + makeE2EHint)
	}
	images, err := parseImages(valueOr(getenv("E2E_IMAGES"), "ubuntu-24.04,ubuntu-26.04"))
	if err != nil {
		return settings{}, err
	}
	s.Images = images
	return s, nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// joinWithAnd joins names as "a", "a and b" or "a, b and c".
func joinWithAnd(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	last := len(names) - 1
	return strings.Join(names[:last], ", ") + " and " + names[last]
}

// parseImages splits a comma-separated list, trims each entry, drops empty ones and checks the form of the rest. An
// image named twice is an error, since two clusters would share a name and a store.
func parseImages(list string) ([]string, error) {
	var images []string
	for _, entry := range strings.Split(list, ",") {
		image := strings.TrimSpace(entry)
		if image == "" {
			continue
		}
		if !imageForm.MatchString(image) {
			return nil, fmt.Errorf("E2E_IMAGES: %q is not of the form ubuntu-NN.NN", image)
		}
		if slices.Contains(images, image) {
			return nil, fmt.Errorf("E2E_IMAGES: %q is named twice", image)
		}
		images = append(images, image)
	}
	if len(images) == 0 {
		return nil, errors.New("E2E_IMAGES names no image")
	}
	return images, nil
}
