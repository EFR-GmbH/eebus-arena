package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

func (c *Controller) assert(runID, nodeID, test, outcome, detail, reference string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	if r == nil {
		return
	}
	a := Assertion{ID: test, Node: nodeID, Outcome: outcome, Detail: detail, Reference: reference, At: time.Now().UTC()}
	r.Assertions = append(r.Assertions, a)
	c.emitLocked(Event{RunID: runID, NodeID: nodeID, Type: "assertion", Data: map[string]any{"id": test, "outcome": outcome, "detail": detail}})
	c.saveLocked(r)
}
func (c *Controller) wait(ctx context.Context, runID string, predicate func(*Run) bool) bool {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() == context.Canceled {
			return false
		}
		c.mu.Lock()
		r := c.runs[runID]
		ok := r != nil && predicate(r)
		c.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
func (c *Controller) Test(runID string) error {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil {
		c.mu.Unlock()
		return fmt.Errorf("run not found")
	}
	if c.closed || c.scenarios[runID] != nil {
		c.mu.Unlock()
		return fmt.Errorf("scenario already running")
	}
	if r.Status != "running" && r.Status != "complete" && r.Status != "interrupted" {
		c.mu.Unlock()
		return fmt.Errorf("start the arena before testing")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.scenarios[runID] = cancel
	r.Status = "testing"
	r.Assertions = []Assertion{}
	c.saveLocked(r)
	definition := r.Definition
	c.workers.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.workers.Done()
		defer func() {
			cleanup, cleanupCancel := context.WithTimeout(c.ctx, 20*time.Second)
			if err := c.restoreRunFaults(cleanup, runID); err != nil {
				c.assert(runID, "", "fault-cleanup", "inconclusive", err.Error(), "")
			}
			cleanupCancel()
			c.mu.Lock()
			delete(c.scenarios, runID)
			r := c.runs[runID]
			if r != nil && r.Status == "testing" {
				if ctx.Err() != nil {
					r.Status = "interrupted"
				} else {
					r.Status = "complete"
				}
			}
			c.saveLocked(r)
			c.emitLocked(Event{RunID: runID, Type: "scenario.finished"})
			c.mu.Unlock()
			cancel()
		}()
		c.scenario(ctx, runID, definition)
	}()
	c.flushSaves()
	return nil
}
func (c *Controller) scenario(ctx context.Context, runID string, d Definition) {
	started := time.Now().UTC()
	readyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	ready := c.wait(readyCtx, runID, func(r *Run) bool {
		for _, n := range r.Nodes {
			if len(n.Capabilities) == 0 || n.Lifecycle != "running" || n.LastSeen.Before(started) {
				return false
			}
		}
		return true
	})
	cancel()
	if !ready {
		c.assert(runID, "", "adapter-ready", "inconclusive", "Not all adapters produced capabilities within 90 seconds", "")
		return
	}
	c.mu.Lock()
	constraintsOK := true
	for _, node := range c.runs[runID].Nodes {
		if node.External == nil && (node.Resources.CPUMillis != nil || node.Resources.MemoryMiB != nil || node.Resources.PIDsLimit != nil) && node.EffectiveResources["verified"] != true {
			constraintsOK = false
		}
	}
	c.mu.Unlock()
	if !constraintsOK {
		c.assert(runID, "", "resource-constraints", "inconclusive", "Requested resource limits were not verified by the runtime", "")
		return
	}
	if d.TimeScale != 1 && d.TimingProfile == "legacy-partial" {
		c.mu.Lock()
		missing := []string{}
		for _, n := range c.runs[runID].Nodes {
			if !Contains(n.Capabilities, "timing.accelerated") {
				missing = append(missing, n.ID)
			}
		}
		c.mu.Unlock()
		if len(missing) > 0 {
			c.assert(runID, "", "timing-profile", "unsupported", "10× timing requires all participating adapters: "+strings.Join(missing, ", "), "")
			return
		}
		c.assert(runID, "", "timing-equivalence", "inconclusive", "Legacy timing scales selected timers only; native SHIP, pairing and discovery clocks are not qualified", "")
	}
	c.mu.Lock()
	nodes := []Node{}
	encoded, _ := json.Marshal(c.runs[runID].Nodes)
	json.Unmarshal(encoded, &nodes)
	c.mu.Unlock()
	// A transport connection is a prerequisite, not proof of use-case readiness.
	if d.Scenario == "explore" {
		c.explore(ctx, runID, d)
		return
	}
	if d.Scenario == "campaign" {
		c.campaign(ctx, runID, d)
		return
	}
	if d.Scenario == "custom" {
		c.steps(ctx, runID, d)
		return
	}
	if d.Scenario == "pairing" {
		c.pairingScenario(ctx, runID, nodes)
		return
	}
	if d.Scenario == "mdns-stress" {
		c.mdnsStressScenario(ctx, runID, d, nodes)
		return
	}
	if !c.pairNodes(ctx, runID, nodes) {
		return
	}
	peerCtx, peerCancel := context.WithTimeout(ctx, 90*time.Second)
	defer peerCancel()
	for _, node := range nodes {
		if ctx.Err() != nil {
			return
		}
		if node.Role != "CS" {
			continue
		}
		ok := c.wait(peerCtx, runID, func(r *Run) bool {
			for _, n := range r.Nodes {
				if n.ID == node.ID && n.Lifecycle == "running" && !n.SnapshotAt.Before(started) {
					if peers, ok := n.Snapshot["peers"].(map[string]any); ok {
						for _, ski := range peerSKIs(*n, nodes) {
							if peers[ski] == "connected" {
								return true
							}
						}
					}
				}
			}
			return false
		})
		if !ok {
			if ctx.Err() != nil {
				c.assert(runID, node.ID, "discovery-trust", "inconclusive", "Discovery interrupted", "SHIP 1.1.0")
				return
			}
			c.assert(runID, node.ID, "discovery-trust", "fail", "No trusted SHIP data-exchange connection within the shared 90-second discovery window", "SHIP 1.1.0")
		} else {
			c.assert(runID, node.ID, "discovery-trust", "pass", "Trusted SHIP connection observed", "SHIP 1.1.0")
		}
	}
	if d.Scenario == "discovery" || d.Scenario == "manual" || ctx.Err() != nil {
		return
	}
	if d.Scenario == "monitoring" {
		c.monitoringScenario(ctx, runID, nodes)
		return
	}
	usecase := "lpc"
	if d.Scenario == "lpp" {
		usecase = "lpp"
	}
	baselineAt := time.Now().UTC()
	var baseline sync.WaitGroup
	parallel := make(chan struct{}, 4)
	partnerCtx, partnerCancel := context.WithTimeout(ctx, 60*time.Second)
	defer partnerCancel()
	for _, node := range nodes {
		if node.Role != "CS" {
			continue
		}
		var guard *Node
		for i := range nodes {
			if nodes[i].Role == "EG" && Contains(peerSKIs(node, nodes), nodes[i].SKI) {
				guard = &nodes[i]
				break
			}
		}
		if guard == nil {
			continue
		}
		select {
		case parallel <- struct{}{}:
		case <-ctx.Done():
			baseline.Wait()
			return
		}
		baseline.Add(1)
		go func(guard, node Node) {
			defer baseline.Done()
			defer func() { <-parallel }()
			if Contains(guard.Capabilities, "observe.partners") {
				ready := c.wait(partnerCtx, runID, func(r *Run) bool {
					for _, n := range r.Nodes {
						if n.ID == guard.ID && n.Lifecycle == "running" && !n.SnapshotAt.Before(started) {
							return nested(n.Snapshot, "partners", usecase, node.SKI) == true
						}
					}
					return false
				})
				if !ready {
					outcome := "fail"
					if ctx.Err() != nil {
						outcome = "inconclusive"
					}
					c.assert(runID, node.ID, "usecase-ready", outcome, "EG did not discover the requested use-case partner within 60 seconds", strings.ToUpper(usecase)+" discovery")
					return
				}
			}
			c.limitScenario(ctx, runID, guard, node, usecase, 1200, true)
			c.limitScenario(ctx, runID, guard, node, usecase, 2400, true)
			if d.Scenario != "heartbeat" {
				c.limitScenario(ctx, runID, guard, node, usecase, 2400, false)
			}
		}(*guard, node)
	}
	baseline.Wait()
	if ctx.Err() != nil {
		return
	}
	if d.Scenario == "heartbeat" {
		c.heartbeatScenario(ctx, runID, d, nodes, baselineAt)
	}
	if d.Scenario == "restart" && len(nodes) > 1 {
		node := nodes[len(nodes)-1]
		before := node.SKI
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.Action(callCtx, runID, node.ID, "restart")
		cancel()
		if err != nil {
			outcome := "fail"
			if ctx.Err() != nil {
				outcome = "inconclusive"
			}
			c.assert(runID, node.ID, "restart", outcome, err.Error(), "")
			return
		}
		waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		ok := c.wait(waitCtx, runID, func(r *Run) bool {
			for _, n := range r.Nodes {
				if n.ID == node.ID {
					peers, _ := n.Snapshot["peers"].(map[string]any)
					connected := false
					for _, ski := range peerSKIs(*n, nodes) {
						connected = connected || peers[ski] == "connected"
					}
					return n.Lifecycle == "running" && n.Incarnation != node.Incarnation && len(n.Capabilities) > 0 && connected
				}
			}
			return false
		})
		cancel()
		c.mu.Lock()
		_, restarted, lookupErr := c.nodeLocked(runID, node.ID)
		var localSki any
		if lookupErr == nil {
			localSki = restarted.Snapshot["localSki"]
		}
		c.mu.Unlock()
		outcome, detail := restartOutcome(ok, ctx.Err(), localSki, before)
		c.assert(runID, node.ID, "restart", outcome, detail, "")
	}
	if d.Scenario == "stress" {
		if d.TimeScale != 1 {
			c.assert(runID, "", "stress-real-time", "unsupported", "Performance acceptance requires real-time timing", "")
			return
		}
		random := rand.New(rand.NewPCG(uint64(d.Seed), 42))
		timer := time.NewTimer(time.Duration(d.DurationSeconds) * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		peak := 0.0
		missing := 0
		samples := 0
		for {
			select {
			case <-ctx.Done():
				c.assert(runID, "", "stress", "inconclusive", "Stress run interrupted", "")
				return
			case <-timer.C:
				outcome := "pass"
				if samples == 0 || missing > 0 {
					outcome = "inconclusive"
				}
				if peak > 12*1024*1024*1024 {
					outcome = "fail"
				}
				c.assert(runID, "", "memory-budget", outcome, fmt.Sprintf("Peak sampled nodes + control/monitoring memory %.2f GiB; 12 GiB budget; %d samples, %d incomplete", peak/(1024*1024*1024), samples, missing), "")
				return
			case <-ticker.C:
				total, complete := c.memorySample(runID)
				samples++
				if !complete {
					missing++
				}
				if total > peak {
					peak = total
				}
				candidates := []Node{}
				for _, n := range nodes {
					if n.Role == "CS" {
						candidates = append(candidates, n)
					}
				}
				if len(candidates) == 0 {
					return
				}
				target := candidates[random.IntN(len(candidates))]
				for _, guard := range nodes {
					if guard.Role == "EG" && Contains(target.TrustedSKIs, guard.SKI) {
						c.limitScenario(ctx, runID, guard, target, "lpc", float64(500+random.IntN(5000)), true)
						break
					}
				}
			}
		}
	}
}
func restartOutcome(reconnected bool, err error, localSki any, expected string) (string, string) {
	detail := "Restart must retain identity, produce a new adapter incarnation, and reconnect to a trusted peer"
	if err != nil {
		return "inconclusive", "Restart observation interrupted"
	}
	if !reconnected {
		return "fail", detail
	}
	if localSki == nil || localSki == "" {
		return "inconclusive", "Reconnected, but adapter did not report its certificate identity"
	}
	if localSki != expected {
		return "fail", "Restarted adapter reported a different certificate identity"
	}
	return "pass", detail
}
func (c *Controller) limitScenario(ctx context.Context, runID string, guard, node Node, uc string, value float64, active bool) {
	if ctx.Err() != nil {
		return
	}
	if uc == "lpp" {
		value = -value
	}
	c.mu.Lock()
	_, receiver, lookupErr := c.nodeLocked(runID, node.ID)
	alreadyMatches := lookupErr == nil && num(nested(receiver.Snapshot, uc, "value")) == value && nested(receiver.Snapshot, uc, "active") == active
	c.mu.Unlock()
	issued := time.Now().UTC()
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	event, err := c.Command(callCtx, runID, guard.ID, Command{Command: "limit.set", Params: map[string]any{"targetSki": node.SKI, "usecase": uc, "value": value, "active": active, "durationSeconds": 600}})
	cancel()
	test := fmt.Sprintf("%s-limit-%g-%t", uc, value, active)
	if err != nil {
		outcome := "inconclusive"
		if strings.Contains(err.Error(), "unsupported") {
			outcome = "unsupported"
		}
		c.assert(runID, node.ID, test, outcome, err.Error(), strings.ToUpper(uc)+" scenario 1")
		return
	}
	if event.Data["ok"] != true {
		outcome := "fail"
		if ctx.Err() != nil {
			outcome = "inconclusive"
		}
		c.assert(runID, node.ID, test, outcome, fmt.Sprint(event.Data["error"]), strings.ToUpper(uc)+" scenario 1")
		return
	}
	if !Contains(node.Capabilities, "observe.limit") {
		c.assert(runID, node.ID, test, "inconclusive", "Receiver limit observation is unavailable", strings.ToUpper(uc)+" scenario 1")
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ok := c.wait(waitCtx, runID, func(r *Run) bool {
		for _, n := range r.Nodes {
			if n.ID == node.ID {
				limit, _ := n.Snapshot[uc].(map[string]any)
				return n.Lifecycle == "running" && !n.LimitAt[uc].Before(issued) && num(limit["value"]) == value && limit["active"] == active
			}
		}
		return false
	})
	cancel()
	outcome := "pass"
	if !ok {
		outcome = "fail"
		if ctx.Err() != nil {
			outcome = "inconclusive"
		}
		if alreadyMatches {
			outcome = "inconclusive"
		}
	}
	c.assert(runID, node.ID, test, outcome, "Receiver must report the requested limit and activation flag", strings.ToUpper(uc)+" scenario 1")
}

// A failsafe state is evidence of heartbeat loss only after a fresh controlled
// state was observed while heartbeats were still enabled.
func observedLPCState(r *Run, id string, since time.Time, states ...string) bool {
	for _, n := range r.Nodes {
		if n.ID != id || n.Lifecycle != "running" || n.SnapshotAt.Before(since) {
			continue
		}
		state := strings.ToUpper(fmt.Sprint(nested(n.Snapshot, "lpc", "state")))
		return Contains(states, state)
	}
	return false
}

func (c *Controller) heartbeatScenario(ctx context.Context, runID string, d Definition, nodes []Node, baselineAt time.Time) {
	eligible := map[string]bool{}
	controlCtx, controlCancel := context.WithTimeout(ctx, 150*time.Second/time.Duration(d.TimeScale))
	for _, node := range nodes {
		if node.Role != "CS" {
			continue
		}
		if !Contains(node.Capabilities, "observe.state") {
			c.assert(runID, node.ID, "failsafe-transition", "unsupported", "Upstream demo does not expose a LPC/LPP state machine", "LPC scenario 3")
			continue
		}
		ok := c.wait(controlCtx, runID, func(r *Run) bool {
			return observedLPCState(r, node.ID, baselineAt, "CONTROLLED", "LIMITED")
		})
		outcome := "pass"
		if !ok {
			outcome = "inconclusive"
		} else {
			eligible[node.ID] = true
		}
		c.assert(runID, node.ID, "heartbeat-positive-control", outcome, "A fresh controlled or limited state is required before suppressing heartbeats", "LPC scenario 3")
	}
	controlCancel()
	if len(eligible) == 0 || ctx.Err() != nil {
		return
	}
	suppressed := map[string]bool{}
	defer func() {
		for _, node := range nodes {
			if node.Role == "EG" {
				callCtx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
				result, err := c.Command(callCtx, runID, node.ID, Command{Command: "heartbeat.set", Params: map[string]any{"enabled": true}})
				cancel()
				if err != nil || result.Data["ok"] != true {
					c.assert(runID, node.ID, "heartbeat-resume", "inconclusive", fmt.Sprintf("Heartbeat restoration was not acknowledged: %v %v", err, result.Data["error"]), "")
				}
			}
		}
	}()
	issued := time.Now().UTC()
	for _, node := range nodes {
		if node.Role != "EG" {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result, err := c.Command(callCtx, runID, node.ID, Command{Command: "heartbeat.set", Params: map[string]any{"enabled": false}})
		cancel()
		suppressed[node.SKI] = err == nil && result.Data["ok"] == true
		if !suppressed[node.SKI] {
			c.assert(runID, node.ID, "heartbeat-suppress", "inconclusive", fmt.Sprintf("Heartbeat suppression was not acknowledged: %v %v", err, result.Data["error"]), "LPC scenario 3")
		}
	}
	failsafeCtx, failsafeCancel := context.WithTimeout(ctx, 150*time.Second/time.Duration(d.TimeScale))
	defer failsafeCancel()
	for _, node := range nodes {
		if !eligible[node.ID] {
			continue
		}
		allSuppressed := len(node.TrustedSKIs) > 0
		for _, ski := range node.TrustedSKIs {
			allSuppressed = allSuppressed && suppressed[ski]
		}
		if !allSuppressed {
			c.assert(runID, node.ID, "failsafe-transition", "inconclusive", "Not all trusted EGs acknowledged heartbeat suppression", "LPC scenario 3")
			continue
		}
		ok := c.wait(failsafeCtx, runID, func(r *Run) bool {
			return observedLPCState(r, node.ID, issued, "FAILSAFE")
		})
		outcome := "pass"
		if !ok {
			outcome = "fail"
			if ctx.Err() != nil {
				outcome = "inconclusive"
			}
		}
		c.assert(runID, node.ID, "failsafe-transition", outcome, "A controlled receiver must enter failsafe after acknowledged heartbeat suppression", "LPC scenario 3")
	}
}
