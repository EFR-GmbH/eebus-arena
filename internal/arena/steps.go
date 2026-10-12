package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (c *Controller) steps(ctx context.Context, runID string, d Definition) {
	for _, step := range d.Steps {
		if ctx.Err() != nil {
			return
		}
		c.Emit(Event{RunID: runID, Type: "scenario.step", Data: map[string]any{"name": step.Name, "action": step.Action, "target": step.Target}})
		if step.Action == "wait" {
			timer := time.NewTimer(time.Duration(step.Seconds) * time.Second / time.Duration(d.TimeScale))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		c.mu.Lock()
		targets := []string{}
		for _, n := range c.runs[runID].Nodes {
			if n.ID == step.Target || n.Group == step.Target {
				targets = append(targets, n.ID)
			}
		}
		c.mu.Unlock()
		for _, node := range targets {
			if ctx.Err() != nil {
				return
			}
			if step.Action == "resource" {
				c.resourceAssertion(ctx, runID, node, step.Name, *step.Resource)
				continue
			}
			issued := time.Now().UTC()
			c.mu.Lock()
			cursor := c.index
			observedID := node
			if step.Expect != nil && step.Expect.Target != "" {
				observedID = step.Expect.Target
			}
			_, observedNode, _ := c.nodeLocked(runID, observedID)
			incarnation := ""
			if observedNode != nil {
				incarnation = observedNode.Incarnation
			}
			c.mu.Unlock()
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			var err error
			if step.Action == "lifecycle" {
				err = c.Action(callCtx, runID, node, step.Command)
			}
			if step.Action == "fault" {
				_, err = c.ApplyNetworkFault(callCtx, runID, *step.Fault)
			}
			if step.Action == "command" {
				var event Event
				event, err = c.Command(callCtx, runID, node, Command{Command: step.Command, Params: step.Params})
				if err == nil && event.Data["ok"] != true {
					err = fmt.Errorf("adapter rejected command: %v", event.Data["error"])
				}
			}
			cancel()
			if err != nil {
				outcome := "fail"
				c.mu.Lock()
				_, n, _ := c.nodeLocked(runID, node)
				unsupported := n != nil && step.Action == "command" && !Contains(n.Capabilities, step.Command)
				c.mu.Unlock()
				if unsupported {
					outcome = "unsupported"
				}
				if ctx.Err() != nil {
					outcome = "inconclusive"
				}
				c.assert(runID, node, step.Name, outcome, err.Error(), "")
				continue
			}
			if e := step.Expect; e != nil {
				scale := d.TimeScale
				if e.RealTime || e.Event != "" || (len(e.Path) > 0 && e.Path[0] == "pairing") {
					scale = 1
				}
				waitCtx, waitCancel := context.WithTimeout(ctx, time.Duration(e.WithinSeconds)*time.Second/time.Duration(scale))
				if e.Event != "" {
					outcome, detail := c.observeEvent(waitCtx, runID, observedID, e, cursor, incarnation)
					waitCancel()
					if ctx.Err() != nil {
						outcome = "inconclusive"
					}
					c.assert(runID, node, step.Name, outcome, detail, "")
					continue
				}
				ok := c.wait(waitCtx, runID, func(r *Run) bool {
					for _, n := range r.Nodes {
						if n.ID == observedID {
							return n.Lifecycle == "running" && !n.SnapshotAt.Before(issued) && jsonEqual(nested(n.Snapshot, e.Path...), e.Equals)
						}
					}
					return false
				})
				waitCancel()
				outcome := "pass"
				if !ok {
					outcome = "fail"
					if ctx.Err() != nil {
						outcome = "inconclusive"
					}
				}
				c.assert(runID, node, step.Name, outcome, fmt.Sprintf("snapshot %v must equal %v", e.Path, e.Equals), "")
			} else {
				outcome := "pass"
				if ctx.Err() != nil {
					outcome = "inconclusive"
				}
				c.assert(runID, node, step.Name, outcome, "Action completed", "")
			}
		}
	}
}
