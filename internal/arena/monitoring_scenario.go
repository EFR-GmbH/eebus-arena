package arena

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Each publish gets a persisted sequence, including subsequent scenario runs.
// Reading the receiver first also excludes coincidentally matching manual input.
func (c *Controller) nextMonitoringValue(runID string, nominal float64, baseline map[string]any) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run := c.runs[runID]
	if run == nil {
		return 0, errors.New("run not found")
	}
	for {
		if run.MonitoringSequence >= 1e12-10000 {
			return 0, errors.New("monitoring input sequence exhausted")
		}
		run.MonitoringSequence++
		value := nominal + math.Copysign(float64(run.MonitoringSequence), nominal)
		if cached, ok := baseline["value"].(float64); ok && math.Abs(cached-value) < 0.001 {
			continue
		}
		c.saveLocked(run)
		return value, nil
	}
}

// Monitoring assertions use the receiver's native SPINE cache. An input
// acknowledgement or the provider's local value cannot satisfy them.
func (c *Controller) monitoringScenario(ctx context.Context, runID string, nodes []Node) {
	for _, provider := range nodes {
		if provider.Monitoring != "mgcp" && provider.Monitoring != "mpc" {
			continue
		}
		for _, receiver := range nodes {
			if receiver.ID == provider.ID || !Contains(peerSKIs(receiver, nodes), provider.SKI) {
				continue
			}
			test := "monitoring-" + provider.ID + "-" + receiver.ID
			if !Contains(provider.Capabilities, "measurement.set") || !Contains(receiver.Capabilities, "measurement.read") {
				c.assert(runID, receiver.ID, test, "unsupported", "Native monitoring controls unavailable for this pair", provider.Monitoring)
				continue
			}
			cases := []struct {
				metric string
				value  float64
			}{{"power", 1234}, {"power", -321}, {"energyConsumed", 4567}, {"energyProduced", -890}}
			for _, input := range cases {
				if ctx.Err() != nil {
					return
				}
				readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				baseline, err := c.Command(readCtx, runID, receiver.ID, Command{Command: "measurement.read"})
				cancel()
				if err != nil || baseline.Data["ok"] != true {
					c.assert(runID, receiver.ID, test+"-"+input.metric+"-baseline", "fail", fmt.Sprintf("Receiver native baseline unavailable: %v", err), provider.Monitoring)
					continue
				}
				cached, _ := nested(baseline.Data, "monitoring", "remote", provider.SKI, provider.Monitoring, input.metric).(map[string]any)
				input.value, err = c.nextMonitoringValue(runID, input.value, cached)
				if err != nil {
					c.assert(runID, receiver.ID, test+"-"+input.metric, "fail", err.Error(), provider.Monitoring)
					continue
				}
				issued := time.Now().UTC()
				commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				result, err := c.Command(commandCtx, runID, provider.ID, Command{Command: "measurement.set", Params: map[string]any{"metric": input.metric, "value": input.value, "state": "normal"}})
				cancel()
				label := fmt.Sprintf("%s-%s-%g", test, input.metric, input.value)
				if err != nil || result.Data["ok"] != true {
					c.assert(runID, receiver.ID, label, "fail", fmt.Sprintf("Provider rejected simulated input: %v %v", err, result.Data["error"]), provider.Monitoring)
					continue
				}
				waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				ok := c.wait(waitCtx, runID, func(run *Run) bool {
					for _, node := range run.Nodes {
						if node.ID != receiver.ID || node.SnapshotAt.Before(issued) || node.Lifecycle != "running" {
							continue
						}
						reading, valid := nested(node.Snapshot, "monitoring", "remote", provider.SKI, provider.Monitoring, input.metric).(map[string]any)
						if !valid || reading["available"] != true || reading["source"] != "native" {
							return false
						}
						value, valid := reading["value"].(float64)
						return valid && math.Abs(value-input.value) < 0.001
					}
					return false
				})
				cancel()
				outcome, detail := "pass", "Receiver observed the transmitted value through its native SPINE cache"
				if !ok {
					outcome, detail = "fail", "Receiver did not observe the transmitted value within 30 seconds"
				}
				if ctx.Err() != nil {
					outcome, detail = "inconclusive", "Monitoring interrupted"
				}
				c.assert(runID, receiver.ID, label, outcome, detail, provider.Monitoring)
			}
		}
	}
}
