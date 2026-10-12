package arena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Exploration uses only explicitly supplied native actions with observed prerequisites.
type Exploration struct {
	Rounds        int    `json:"rounds"`
	Actions       []Step `json:"actions"`
	Invariants    []Step `json:"invariants"`
	SettleSeconds int    `json:"settleSeconds"`
}

// Options refer to retained definition actions without repeating their parameters.
type explorationOption struct {
	ActionIndex int    `json:"actionIndex"`
	Target      string `json:"target"`
}

func (d *Definition) validateExploration(variants map[string]Variant) error {
	if d.Exploration == nil {
		if d.Scenario == "explore" {
			return errors.New("explore requires a model")
		}
		return nil
	}
	e := d.Exploration
	if d.Scenario != "explore" || e.Rounds < 1 || e.Rounds > 200 || len(e.Actions) < 1 || len(e.Actions) > 100 || len(e.Invariants) > 20 || e.SettleSeconds < 1 || e.SettleSeconds > 60 {
		return errors.New("exploration needs 1–200 rounds, 1–100 actions, at most 20 invariants and 1–60 settle seconds")
	}
	copy := *d
	copy.Exploration = nil
	copy.Scenario = "custom"
	copy.Steps = append(append([]Step{}, e.Actions...), e.Invariants...)
	for _, action := range e.Actions {
		encoded, err := json.Marshal(action)
		if err != nil || len(encoded) > 16<<10 {
			return errors.New("each exploration action must fit in 16 KiB")
		}
		if !Contains([]string{"command", "lifecycle"}, action.Action) {
			return errors.New("exploration actions must be native commands or lifecycle changes")
		}
	}
	for _, invariant := range e.Invariants {
		if invariant.Action != "assert" || invariant.Expect == nil || invariant.Expect.Absent {
			return errors.New("exploration invariants must assert positive native observations")
		}
	}
	return copy.Validate(variants)
}
func (c *Controller) explore(ctx context.Context, run string, d Definition) {
	random := rand.New(rand.NewPCG(uint64(d.Seed), 0x4152454e41))
	for round := 0; round < d.Exploration.Rounds; round++ {
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		nodes := []Node{}
		if r := c.runs[run]; r != nil {
			blob, _ := json.Marshal(r.Nodes)
			_ = json.Unmarshal(blob, &nodes)
		}
		c.mu.Unlock()
		enabled := []Step{}
		options := []explorationOption{}
		for index, action := range d.Exploration.Actions {
			for _, node := range nodes {
				if node.ID != action.Target && node.Group != action.Target {
					continue
				}
				valid := node.Lifecycle == "running" && time.Since(node.LastSeen) < 10*time.Second
				if action.Action == "command" {
					valid = valid && Contains(node.Capabilities, action.Command)
					if peer, ok := action.Params["targetNode"].(string); ok {
						peerValid := false
						for _, other := range nodes {
							if other.ID == peer && other.Lifecycle == "running" && time.Since(other.LastSeen) < 10*time.Second {
								peerValid = observedTrust(&node, &other)
								if action.Command == "pairing.announce" || action.Command == "pairing.stop" {
									peerValid = Contains(node.PairingPeers, peer)
								}
							}
						}
						valid = valid && peerValid
					}
				}
				if action.Action == "lifecycle" {
					valid = Contains([]string{"running", "exited", "paused"}, node.Lifecycle)
					if action.Command == "start" {
						valid = node.Lifecycle == "exited"
					}
					if action.Command == "resume" {
						valid = node.Lifecycle == "paused"
					}
					if action.Command == "stop" || action.Command == "restart" || action.Command == "pause" || action.Command == "kill" {
						valid = node.Lifecycle == "running"
					}
					if node.External != nil && Contains([]string{"pause", "resume"}, action.Command) {
						valid = false
					}
				}
				if valid {
					copy := action
					copy.Target = node.ID
					enabled = append(enabled, copy)
					options = append(options, explorationOption{ActionIndex: index, Target: node.ID})
				}
			}
		}
		if len(enabled) == 0 {
			c.assert(run, "", "exploration-enabled", "inconclusive", "No native action has its declared capability and observed prerequisites", "")
			return
		}
		choice := random.IntN(len(enabled))
		action := enabled[choice]
		c.Emit(Event{RunID: run, NodeID: action.Target, Type: "exploration.choice", Data: map[string]any{"seed": d.Seed, "round": round, "choice": choice, "enabled": options, "action": action}})
		copy := d
		copy.Exploration = nil
		copy.Steps = []Step{action}
		c.steps(ctx, run, copy)
		timer := time.NewTimer(time.Duration(d.Exploration.SettleSeconds) * time.Second / time.Duration(d.TimeScale))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		copy.Steps = append([]Step(nil), d.Exploration.Invariants...)
		for i := range copy.Steps {
			copy.Steps[i].Name = fmt.Sprintf("round-%03d/%s", round, copy.Steps[i].Name)
		}
		c.steps(ctx, run, copy)
		c.mu.Lock()
		failed := false
		if r := c.runs[run]; r != nil {
			for _, assertion := range r.Assertions {
				if assertion.Outcome == "fail" {
					failed = true
				}
			}
		}
		c.mu.Unlock()
		if failed {
			return
		}
	}
}
