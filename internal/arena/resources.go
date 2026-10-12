package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// sampleControl measures the controller and monitoring containers for the
// memory budget.
func (c *Controller) sampleControl() {
	ctx, cancel := context.WithTimeout(c.ctx, 4*time.Second)
	defer cancel()
	filter, _ := json.Marshal(map[string][]string{"label": {"org.eebus-arena.control=true"}})
	var containers []map[string]any
	if c.docker == nil {
		return
	}
	if err := c.docker.JSON(ctx, "GET", "/containers/json?filters="+url.QueryEscape(string(filter)), nil, &containers); err != nil {
		return
	}
	total := 0.0
	valid := true
	orchestrator := false
	for _, container := range containers {
		var stat map[string]any
		if err := c.docker.JSON(ctx, "GET", "/containers/"+fmt.Sprint(container["Id"])+"/stats?stream=false&one-shot=true", nil, &stat); err != nil {
			valid = false
			continue
		}
		total += num(nested(stat, "memory_stats", "usage"))
		if nested(stat, "memory_stats", "usage") == nil {
			valid = false
		}
		labels, _ := container["Labels"].(map[string]any)
		if labels["com.docker.compose.service"] == "arena" {
			orchestrator = true
		}
	}
	if !orchestrator {
		data, err := os.ReadFile("/proc/self/statm")
		fields := strings.Fields(string(data))
		if err == nil && len(fields) > 1 {
			pages, e := strconv.ParseFloat(fields[1], 64)
			if e == nil {
				total += pages * float64(os.Getpagesize())
			} else {
				valid = false
			}
		} else {
			valid = false
		}
	}
	c.mu.Lock()
	c.controlMetrics = map[string]any{"memoryBytes": total, "complete": valid, "at": time.Now().UTC()}
	c.mu.Unlock()
}

func (c *Controller) memorySample(runID string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := num(c.controlMetrics["memoryBytes"])
	complete := c.controlMetrics["complete"] == true
	if at, ok := c.controlMetrics["at"].(time.Time); !ok || time.Since(at) > 10*time.Second {
		complete = false
	}
	for _, n := range c.runs[runID].Nodes {
		if n.Lifecycle != "running" {
			complete = false
		}
		if n.Metrics["memoryBytes"] == nil {
			complete = false
		}
		if !sampleFresh(n.Metrics) {
			complete = false
		}
		total += num(n.Metrics["memoryBytes"])
	}
	return total, complete
}

func sampleFresh(metrics map[string]any) bool {
	at, _ := metrics["at"].(time.Time)
	if text, ok := metrics["at"].(string); ok {
		at, _ = time.Parse(time.RFC3339Nano, text)
	}
	return !at.IsZero() && time.Since(at) <= 10*time.Second
}
