package arena

import (
	"context"
	"strings"
	"time"
)

// observeEvent checks a bounded interval and refuses to infer absence across gaps.
func (c *Controller) observeEvent(ctx context.Context, runID, nodeID string, e *Expectation, after uint64, incarnation string) (string, string) {
	c.mu.Lock()
	_, n, err := c.nodeLocked(runID, nodeID)
	capability := "observe." + strings.Split(e.Event, ".")[0]
	supported := err == nil && Contains(n.Capabilities, capability)
	c.mu.Unlock()
	if !supported {
		return "unsupported", "Missing " + capability
	}
	cursor := after
	seen := false
	gap := false
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.mu.Lock()
		_, node, err := c.nodeLocked(runID, nodeID)
		if err != nil || node.Incarnation != incarnation || node.Lifecycle != "running" || time.Since(node.LastSeen) > 10*time.Second {
			gap = true
		}
		if len(c.events) > 0 && cursor+1 < c.events[0].Index {
			gap = true
		}
		for _, event := range c.events {
			if event.Index <= cursor {
				continue
			}
			cursor = event.Index
			if event.RunID != runID {
				continue
			}
			if event.Type == "telemetry.gap" || event.Type == "evidence.rotated" || event.Type == "telemetry.disconnected" || event.Type == "pairing.suppressed" {
				gap = true
			}
			if event.NodeID != nodeID || event.Incarnation != incarnation || event.Type != e.Event {
				continue
			}
			matches := true
			for key, value := range e.Match {
				if !jsonEqual(event.Data[key], value) {
					matches = false
					break
				}
			}
			seen = seen || matches
		}
		c.mu.Unlock()
		if seen {
			if e.Absent {
				return "fail", "Forbidden native event observed: " + e.Event
			}
			return "pass", "Native event observed: " + e.Event
		}
		select {
		case <-ctx.Done():
			if gap {
				return "inconclusive", "Observation interval contains missing telemetry or a changed incarnation"
			}
			if ctx.Err() == context.Canceled {
				return "inconclusive", "Observation interrupted"
			}
			if e.Absent {
				return "pass", "No matching native event during the complete observation interval"
			}
			return "fail", "Expected native event was not observed: " + e.Event
		case <-ticker.C:
		}
	}
}
