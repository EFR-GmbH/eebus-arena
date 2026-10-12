package arena

import (
	"context"
	"fmt"
	"time"
)

func peerSKIs(n Node, nodes []Node) []string {
	skis := append([]string(nil), n.TrustedSKIs...)
	for _, peer := range nodes {
		if Contains(n.IntendedPeers, peer.ID) && !Contains(skis, peer.SKI) {
			skis = append(skis, peer.SKI)
		}
	}
	return skis
}

func (c *Controller) pairNodes(ctx context.Context, runID string, nodes []Node) bool {
	all := true
	counts := map[string]int{}
	for _, listener := range nodes {
		if listener.Role != "CS" || len(listener.PairingPeers) == 0 {
			continue
		}
		var announcer *Node
		for i := range nodes {
			if nodes[i].Role == "EG" && Contains(listener.PairingPeers, nodes[i].ID) {
				announcer = &nodes[i]
				break
			}
		}
		if announcer == nil {
			continue
		}
		if !Contains(announcer.Capabilities, "pairing.announce") || !Contains(listener.Capabilities, "pairing.listen") || !Contains(listener.Capabilities, "observe.pairing") {
			c.assert(runID, listener.ID, "pairing-capability", "unsupported", "Native announcer, listener and trust observations required", "")
			all = false
			continue
		}
		counts[announcer.ID]++
		if counts[announcer.ID] > 1 && Contains(announcer.Capabilities, "pairing.single-target") {
			c.assert(runID, listener.ID, "pairing-capacity", "unsupported", "Native adapter supports one announcement target at a time", "")
			all = false
			continue
		}
		issued := time.Now().UTC()
		callCtx, callCancel := context.WithTimeout(ctx, 20*time.Second)
		result, err := c.Command(callCtx, runID, announcer.ID, Command{Command: "pairing.announce", Params: map[string]any{"targetNode": listener.ID}})
		callCancel()
		if err != nil || result.Data["ok"] != true {
			c.assert(runID, listener.ID, "pairing-start", "inconclusive", fmt.Sprintf("Announcement not acknowledged: %v %v", err, result.Data["error"]), "")
			all = false
			continue
		}
		waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		ready := c.wait(waitCtx, runID, func(r *Run) bool {
			var a, b *Node
			for _, n := range r.Nodes {
				if n.ID == announcer.ID {
					a = n
				}
				if n.ID == listener.ID {
					b = n
				}
			}
			return a != nil && b != nil && a.Lifecycle == "running" && b.Lifecycle == "running" && a.SnapshotAt.After(issued) && b.SnapshotAt.After(issued) && observedTrust(b, a)
		})
		cancel()
		outcome := "pass"
		if !ready {
			outcome = "fail"
			all = false
		}
		if ctx.Err() != nil {
			outcome = "inconclusive"
		}
		c.assert(runID, listener.ID, "pairing-trust", outcome, "Listener must report native learned trust after announcement", "")
	}
	return all
}

func (c *Controller) pairingScenario(ctx context.Context, runID string, nodes []Node) {
	if !c.pairNodes(ctx, runID, nodes) {
		return
	}
	for _, listener := range nodes {
		if listener.Role != "CS" {
			continue
		}
		for _, announcer := range nodes {
			if announcer.Role != "EG" || !Contains(listener.PairingPeers, announcer.ID) {
				continue
			}
			since := time.Now().UTC()
			waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			ready := c.wait(waitCtx, runID, func(r *Run) bool {
				count := 0
				for _, n := range r.Nodes {
					if n.ID == announcer.ID && n.SnapshotAt.After(since) && nested(n.Snapshot, "peers", listener.SKI) == "connected" {
						count++
					}
					if n.ID == listener.ID && n.SnapshotAt.After(since) && nested(n.Snapshot, "peers", announcer.SKI) == "connected" {
						count++
					}
				}
				return count == 2
			})
			cancel()
			outcome := "pass"
			if !ready {
				outcome = "fail"
			}
			if ctx.Err() != nil {
				outcome = "inconclusive"
			}
			c.assert(runID, listener.ID, "pairing-connection", outcome, "Both peers must report SHIP data exchange", "")
			if !ready {
				break
			}
			for _, uc := range []string{"lpc", "lpp"} {
				waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				compatible := c.wait(waitCtx, runID, func(r *Run) bool {
					for _, n := range r.Nodes {
						if n.ID == announcer.ID {
							return n.SnapshotAt.After(since) && nested(n.Snapshot, "partners", uc, listener.SKI) == true
						}
					}
					return false
				})
				cancel()
				outcome := "pass"
				if !compatible {
					outcome = "fail"
				}
				if !Contains(announcer.Capabilities, "observe.partners") {
					outcome = "unsupported"
				}
				if ctx.Err() != nil {
					outcome = "inconclusive"
				}
				c.assert(runID, listener.ID, "pairing-"+uc+"-ready", outcome, "Native use-case partner discovery", "")
				if compatible {
					value := 1200.0
					if uc == "lpp" {
						value = -value
					}
					c.limitScenario(ctx, runID, announcer, listener, uc, value, true)
					c.limitScenario(ctx, runID, announcer, listener, uc, value, false)
				}
			}
			break // Additional EG links are replacement candidates, not simultaneous pairings.
		}
	}
}
