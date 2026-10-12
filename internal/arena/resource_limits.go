package arena

import (
	"errors"
	"fmt"
)

// ResourceLimits uses nil to inherit and zero to explicitly remove a limit.
// CPU is measured in thousandths of one logical CPU; memory is in MiB.
type ResourceLimits struct {
	CPUMillis *int `json:"cpuMillis,omitempty"`
	MemoryMiB *int `json:"memoryMiB,omitempty"`
	PIDsLimit *int `json:"pidsLimit,omitempty"`
}

// NodeOverride changes individual nodes without splitting their group.
type NodeOverride struct {
	Resources  ResourceLimits `json:"resources"`
	Monitoring *string        `json:"monitoring,omitempty"`
}

// SamplingProfile configures real-time collection independently of protocol time.
type SamplingProfile struct {
	IntervalMillis  int      `json:"intervalMillis"`
	DiagnosticNodes []string `json:"diagnosticNodes,omitempty"`
}

func (r ResourceLimits) validate() error {
	for _, setting := range []struct {
		name     string
		value    *int
		min, max int
	}{
		{"cpuMillis", r.CPUMillis, 10, 256000},
		{"memoryMiB", r.MemoryMiB, 16, 1048576},
		{"pidsLimit", r.PIDsLimit, 16, 65536},
	} {
		if setting.value != nil && *setting.value != 0 && (*setting.value < setting.min || *setting.value > setting.max) {
			return fmt.Errorf("%s must be 0 (unlimited) or %d–%d", setting.name, setting.min, setting.max)
		}
	}
	return nil
}

func validateGroupResources(g Group) error {
	if g.Resources == nil {
		return nil
	}
	if g.MemoryMB != 0 && g.Resources.MemoryMiB != nil {
		return errors.New("memoryMB and resources.memoryMiB cannot both be set")
	}
	return g.Resources.validate()
}

func (d Definition) hasNode(id string) bool {
	for _, g := range d.Groups {
		for i := 1; i <= g.Count; i++ {
			if id == fmt.Sprintf("%s-%03d", g.ID, i) {
				return true
			}
		}
	}
	return false
}

func (d Definition) validateResourceSettings() error {
	for node, override := range d.NodeOverrides {
		if !d.hasNode(node) {
			return fmt.Errorf("unknown resource override node %s", node)
		}
		if err := override.Resources.validate(); err != nil {
			return fmt.Errorf("node %s: %w", node, err)
		}
	}
	if s := d.Sampling; s != nil {
		if s.IntervalMillis != 1000 && s.IntervalMillis != 2000 && s.IntervalMillis != 5000 {
			return errors.New("sampling intervalMillis must be 1000, 2000 or 5000")
		}
		if len(s.DiagnosticNodes) > 8 {
			return errors.New("250 ms diagnostic collection is limited to eight nodes")
		}
		seen := map[string]bool{}
		for _, node := range s.DiagnosticNodes {
			if !d.hasNode(node) || seen[node] {
				return errors.New("diagnostic nodes must be distinct existing node IDs")
			}
			seen[node] = true
		}
	}
	return nil
}

func (d Definition) resourcesFor(node, group string) ResourceLimits {
	r := ResourceLimits{}
	for _, g := range d.Groups {
		if g.ID != group {
			continue
		}
		if g.MemoryMB > 0 {
			value := g.MemoryMB
			r.MemoryMiB = &value
		}
		if g.Resources != nil {
			r = r.overlay(*g.Resources)
		}
	}
	if override, ok := d.NodeOverrides[node]; ok {
		r = r.overlay(override.Resources)
	}
	return r
}

func (r ResourceLimits) overlay(other ResourceLimits) ResourceLimits {
	if other.CPUMillis != nil {
		value := *other.CPUMillis
		r.CPUMillis = &value
	}
	if other.MemoryMiB != nil {
		value := *other.MemoryMiB
		r.MemoryMiB = &value
	}
	if other.PIDsLimit != nil {
		value := *other.PIDsLimit
		r.PIDsLimit = &value
	}
	return r
}

func (d Definition) samplingInterval(node string) int {
	if d.Sampling != nil {
		if Contains(d.Sampling.DiagnosticNodes, node) {
			return 250
		}
		return d.Sampling.IntervalMillis
	}
	return 1000
}

func (r ResourceLimits) applyCompose(service map[string]any) {
	if r.CPUMillis != nil && *r.CPUMillis > 0 {
		service["cpus"] = float64(*r.CPUMillis) / 1000
	}
	if r.MemoryMiB != nil && *r.MemoryMiB > 0 {
		service["mem_limit"] = fmt.Sprintf("%dm", *r.MemoryMiB)
		// Disable swap beyond the RAM allocation; the effective runtime config is inspected.
		service["memswap_limit"] = fmt.Sprintf("%dm", *r.MemoryMiB)
	}
	if r.PIDsLimit != nil && *r.PIDsLimit > 0 {
		service["pids_limit"] = *r.PIDsLimit
	}
}
