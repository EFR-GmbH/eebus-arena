package arena

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ClockQualification is a build-attested native clock coverage declaration.
// A broad legacy capability is deliberately insufficient for a reliable profile.
type ClockQualification struct {
	Version        int      `json:"version"`
	Factors        []int    `json:"factors"`
	Profiles       []string `json:"profiles"`
	Coverage       []string `json:"coverage"`
	Evidence       string   `json:"evidence"`
	MaxDriftMillis float64  `json:"maxDriftMillis"`
}

var clockAreas = []string{"ship", "pairing", "spine", "discovery", "dependencies", "persistence"}

func clockQualified(q ClockQualification, rate int, profile string) bool {
	if q.Version != 1 || q.Evidence == "" || q.MaxDriftMillis <= 0 {
		return false
	}
	hasRate := false
	for _, factor := range q.Factors {
		hasRate = hasRate || factor == rate
	}
	if !hasRate || !Contains(q.Profiles, profile) {
		return false
	}
	for _, area := range clockAreas {
		if !Contains(q.Coverage, area) {
			return false
		}
	}
	return true
}
func (c *Controller) clockPreflightLocked(r *Run) error {
	if r.Definition.TimeScale == 1 {
		return nil
	}
	if r.Definition.TimingProfile == "legacy-partial" {
		return errors.New("legacy partial timing is not a reliable clock profile; choose 1×")
	}
	missing := []string{}
	for _, node := range r.Nodes {
		var provenance struct {
			Clock ClockQualification `json:"clock"`
		}
		variant, ok := c.variants[node.Variant]
		if ok {
			_ = json.Unmarshal(variant.Provenance, &provenance)
		}
		if node.External != nil || !clockQualified(provenance.Clock, r.Definition.TimeScale, r.Definition.Scenario) {
			missing = append(missing, node.ID)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("reliable %d× timing unsupported for %s: complete native clock and dependency qualification is missing; choose 1×", r.Definition.TimeScale, strings.Join(missing, ", "))
	}
	return nil
}
