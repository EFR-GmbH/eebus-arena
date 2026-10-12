package arena

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type arenaFeatures struct {
	NetworkModes        []string `json:"networkModes"`
	MonitoringSelection int      `json:"monitoringSelection"`
	MonitoringProviders []string `json:"monitoringProviders"`
}

func featuresFor(variant Variant) arenaFeatures {
	var provenance struct {
		Features arenaFeatures `json:"arenaFeatures"`
	}
	if json.Unmarshal(variant.Provenance, &provenance) != nil {
		return arenaFeatures{}
	}
	return provenance.Features
}

func (d Definition) monitoringFor(node, group string) string {
	if override := d.NodeOverrides[node]; override.Monitoring != nil {
		return *override.Monitoring
	}
	for _, g := range d.Groups {
		if g.ID == group {
			return g.Monitoring
		}
	}
	return ""
}

func (d Definition) validateMonitoring(variants map[string]Variant) error {
	providers := 0
	for _, group := range d.Groups {
		if !Contains([]string{"", "none", "mpc", "mgcp"}, group.Monitoring) {
			return errors.New("monitoring must be none, mpc or mgcp")
		}
		for i := 1; i <= group.Count; i++ {
			node := fmt.Sprintf("%s-%03d", group.ID, i)
			selection := d.monitoringFor(node, group.ID)
			if !Contains([]string{"", "none", "mpc", "mgcp"}, selection) {
				return fmt.Errorf("node %s: monitoring must be none, mpc or mgcp", node)
			}
			if selection == "" || selection == "none" {
				continue
			}
			if group.Role == "STRESSER" {
				return errors.New("mDNS stressers cannot offer EEBUS monitoring use cases")
			}
			variant := variants[group.Variant]
			if !Contains(featuresFor(variant).MonitoringProviders, selection) {
				return fmt.Errorf("variant %s has no selectable native %s provider; rebuild a supported stack", variant.ID, selection)
			}
			if selection == "mgcp" {
				if group.Role != "EG" {
					return errors.New("MGCP can only be offered by an EG")
				}
				providers++
			}
		}
	}
	if providers > 1 {
		return errors.New("an arena can have at most one MGCP provider")
	}
	if d.Scenario == "monitoring" {
		selected := false
		for _, group := range d.Groups {
			for i := 1; i <= group.Count; i++ {
				value := d.monitoringFor(fmt.Sprintf("%s-%03d", group.ID, i), group.ID)
				selected = selected || value == "mgcp" || value == "mpc"
			}
		}
		if !selected || d.TimeScale != 1 {
			return errors.New("monitoring scenario needs a selected provider and real-time timing")
		}
	}
	if providers > 0 {
		for _, group := range d.Groups {
			variant := variants[group.Variant]
			if variant.Stack == "openeebus" && featuresFor(variant).MonitoringSelection != 1 {
				return errors.New("MGCP arenas require rebuilt C variants without implicit example providers")
			}
		}
	}
	return nil
}

// MonitoringObservation is a retained display sample. Scenario assertions only
// read live Snapshot/SnapshotAt and cannot satisfy freshness from this history.
type MonitoringObservation struct {
	At          time.Time      `json:"at"`
	Incarnation string         `json:"incarnation"`
	Values      map[string]any `json:"values"`
}
