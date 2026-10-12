package arena

import (
	"context"
	"fmt"
	"time"
)

func (c *Controller) mdnsStressScenario(ctx context.Context, runID string, d Definition, nodes []Node) {
	stressers := []Node{}
	for _, n := range nodes {
		if n.Role == "STRESSER" {
			stressers = append(stressers, n)
		}
	}
	if len(stressers) == 0 {
		c.assert(runID, "", "mdns-stresser", "unsupported", "Add an mDNS stresser node", "")
		return
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, n := range stressers {
			result, err := c.Command(cleanup, runID, n.ID, Command{Command: "mdns-stress.stop"})
			if err != nil || result.Data["ok"] != true {
				c.assert(runID, n.ID, "mdns-cleanup", "inconclusive", "Stresser stop was not acknowledged", "")
			}
		}
	}()
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		peak, samples, missing := 0.0, 0, 0
		for {
			select {
			case <-monitorCtx.Done():
				outcome := "pass"
				if samples == 0 || missing > 0 || ctx.Err() != nil {
					outcome = "inconclusive"
				}
				if peak > 12*1024*1024*1024 {
					outcome = "fail"
				}
				c.assert(runID, "", "mdns-memory-budget", outcome, fmt.Sprintf("Peak nodes + control/monitoring memory %.2f GiB; 12 GiB budget; %d samples, %d incomplete", peak/(1024*1024*1024), samples, missing), "")
				return
			case <-ticker.C:
				total, complete := c.memorySample(runID)
				samples++
				if !complete {
					missing++
				}
				peak = max(peak, total)
			}
		}
	}()
	defer func() { stopMonitor(); <-monitorDone }()
	for _, phase := range []struct {
		name           string
		rate, services int
	}{{"baseline", 0, 0}, {"ramp-10", 10, 100}, {"ramp-100", 100, 1000}, {"sustained", 1000, 1000}, {"recovery", 0, 0}} {
		if ctx.Err() != nil {
			return
		}
		c.Emit(Event{RunID: runID, Type: "scenario.phase", Data: map[string]any{"name": phase.name, "rate": phase.rate, "services": phase.services}})
		seconds := min(30, d.DurationSeconds)
		if phase.name == "sustained" {
			seconds = d.DurationSeconds
		}
		for _, n := range stressers {
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := c.Command(call, runID, n.ID, Command{Command: "mdns-stress.stop"})
			cancel()
			if err != nil || result.Data["ok"] != true {
				c.assert(runID, n.ID, "mdns-stop", "inconclusive", "Could not establish phase boundary", "")
				return
			}
			if phase.rate > 0 {
				profile := StressProfile{Version: 1, Seed: d.Seed, Services: phase.services, Rate: phase.rate, Burst: 100, TTL: 120, Churn: 20, DurationSeconds: min(86400, seconds+900), Pattern: "churn", Service: "_ship._tcp"}
				for _, g := range d.Groups {
					if g.ID == n.Group && g.Stress != nil {
						profile = *g.Stress
						profile.Rate = phase.rate
						profile.Services = phase.services
						profile.DurationSeconds = min(86400, seconds+900)
					}
				}
				call, cancel = context.WithTimeout(ctx, 10*time.Second)
				result, err = c.Command(call, runID, n.ID, Command{Command: "mdns-stress.start", Params: map[string]any{"profile": profile}})
				cancel()
				if err != nil || result.Data["ok"] != true {
					c.assert(runID, n.ID, "mdns-start", "inconclusive", fmt.Sprintf("Workload not acknowledged: %v %v", err, result.Data["error"]), "")
					return
				}
			}
		}
		c.pairingScenario(ctx, runID, nodes)
		// Exercise pre-trusted peers too, including jEEBUS.
		for _, cs := range nodes {
			if cs.Role != "CS" || len(cs.PairingPeers) > 0 {
				continue
			}
			for _, eg := range nodes {
				if eg.Role == "EG" && Contains(cs.TrustedSKIs, eg.SKI) {
					c.limitScenario(ctx, runID, eg, cs, "lpc", 1200, true)
					c.limitScenario(ctx, runID, eg, cs, "lpp", -1200, true)
					break
				}
			}
		}
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if phase.rate > 0 {
			for _, n := range stressers {
				c.mu.Lock()
				_, current, _ := c.nodeLocked(runID, n.ID)
				fresh := current != nil && time.Since(current.SnapshotAt) < 5*time.Second
				var sent, errors, skipped float64
				active := false
				if current != nil {
					active = nested(current.Snapshot, "mdns", "active") == true
					sent, errors, skipped = num(nested(current.Snapshot, "mdns", "sent")), num(nested(current.Snapshot, "mdns", "sendErrors")), num(nested(current.Snapshot, "mdns", "skipped"))
				}
				c.mu.Unlock()
				outcome := "pass"
				if !fresh || !active {
					outcome = "inconclusive"
				} else if sent == 0 || errors > 0 || skipped > 0 {
					outcome = "fail"
				}
				c.assert(runID, n.ID, "mdns-delivery-"+phase.name, outcome, fmt.Sprintf("Socket writes %.0f; errors %.0f; skipped %.0f; target %d packets/s; active=%t (not proof of delivery)", sent, errors, skipped, phase.rate, active), "")
			}
		}
	}
}
