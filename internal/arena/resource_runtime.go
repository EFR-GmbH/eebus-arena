package arena

import (
	"context"
	"fmt"
	"math"
	"time"
)

func (c *Controller) inspectResourceLimits(ctx context.Context, run *Run, generation uint64, node, container string) {
	c.mu.Lock()
	r, n, err := c.nodeLocked(run.ID, node)
	if err != nil || r != run || r.generation != generation || n.Container != container || n.EffectiveResources != nil && n.EffectiveResources["lifecycle"] == n.Lifecycle {
		c.mu.Unlock()
		return
	}
	wanted := r.Definition.resourcesFor(node, n.Group)
	lifecycle := n.Lifecycle
	c.mu.Unlock()
	var info struct {
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			Memory     int64
			MemorySwap int64
			NanoCpus   int64
			CpuQuota   int64
			CpuPeriod  int64
			PidsLimit  *int64
			CpusetCpus string
		}
		State struct{ OOMKilled bool }
	}
	if err := c.docker.JSON(ctx, "GET", "/containers/"+container+"/json", nil, &info); err != nil {
		return
	}
	if info.Config.Labels["org.eebus-arena.run"] != run.ID || info.Config.Labels["org.eebus-arena.node"] != node {
		return
	}
	cpu := float64(info.HostConfig.NanoCpus) / 1e6
	if cpu == 0 && info.HostConfig.CpuQuota > 0 && info.HostConfig.CpuPeriod > 0 {
		cpu = float64(info.HostConfig.CpuQuota) / float64(info.HostConfig.CpuPeriod) * 1000
	}
	pids := int64(0)
	if info.HostConfig.PidsLimit != nil && *info.HostConfig.PidsLimit > 0 {
		pids = *info.HostConfig.PidsLimit
	}
	verified := true
	if wanted.CPUMillis != nil && math.Abs(cpu-float64(*wanted.CPUMillis)) > 0.01 {
		verified = false
	}
	if wanted.MemoryMiB != nil && info.HostConfig.Memory != int64(*wanted.MemoryMiB)*(1<<20) {
		verified = false
	}
	if wanted.MemoryMiB != nil && *wanted.MemoryMiB > 0 && info.HostConfig.MemorySwap != info.HostConfig.Memory {
		verified = false
	}
	if wanted.PIDsLimit != nil && pids != int64(*wanted.PIDsLimit) {
		verified = false
	}
	effective := map[string]any{"cpuMillis": cpu, "memoryBytes": info.HostConfig.Memory, "memorySwapBytes": info.HostConfig.MemorySwap, "pidsLimit": pids, "cpuset": info.HostConfig.CpusetCpus, "verified": verified, "container": container, "lifecycle": lifecycle, "oomKilled": info.State.OOMKilled, "at": time.Now().UTC()}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, n, err = c.nodeLocked(run.ID, node)
	if err != nil || c.closed || r != run || r.generation != generation || n.Container != container {
		return
	}
	n.Resources = wanted
	n.EffectiveResources = effective
	c.dirty[r.ID] = true
	c.emitLocked(Event{RunID: r.ID, NodeID: node, Type: "resource.limits", Data: effective})
	if !verified {
		c.emitLocked(Event{RunID: r.ID, NodeID: node, Type: "resource.constraint_failed", Data: map[string]any{"error": fmt.Sprintf("runtime limits differ from requested limits for %s", node)}})
	}
	if info.State.OOMKilled {
		c.emitLocked(Event{RunID: r.ID, NodeID: node, Type: "resource.oom", Data: map[string]any{"source": "Docker OOMKilled"}})
	}
}
