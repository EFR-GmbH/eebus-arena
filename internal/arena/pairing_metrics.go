package arena

import (
	"fmt"
	"time"
)

// PairingProgress separates command issuance from native trust, transport and use-case readiness.
type PairingProgress struct {
	CommandID string             `json:"commandId"`
	Started   time.Time          `json:"started"`
	Stages    map[string]float64 `json:"stages"`
}

// PairingMetric is a bounded-cardinality cumulative latency histogram in seconds.
type PairingMetric struct {
	Count   uint64    `json:"count"`
	Sum     float64   `json:"sum"`
	Buckets [7]uint64 `json:"buckets"`
}

var pairingBounds = [6]float64{1, 5, 10, 30, 60, 90}

func beginPairing(n *Node, peerID, commandID string, at time.Time) {
	if n.PairingProgress == nil {
		n.PairingProgress = map[string]*PairingProgress{}
	}
	n.PairingProgress[peerID] = &PairingProgress{CommandID: commandID, Started: at, Stages: map[string]float64{}}
}
func (c *Controller) observePairingProgress(r *Run, n *Node) {
	if n.Lifecycle != "running" {
		return
	}
	for peerID, progress := range n.PairingProgress {
		if n.SnapshotAt.Before(progress.Started) {
			continue
		}
		var peer *Node
		for _, candidate := range r.Nodes {
			if candidate.ID == peerID {
				peer = candidate
				break
			}
		}
		if peer == nil {
			continue
		}
		stages := map[string]bool{"trust": observedTrust(n, peer), "ship": nested(n.Snapshot, "peers", peer.SKI) == "connected", "lpc": nested(n.Snapshot, "partners", "lpc", peer.SKI) == true, "lpp": nested(n.Snapshot, "partners", "lpp", peer.SKI) == true}
		for stage, ready := range stages {
			if !ready {
				continue
			}
			if _, done := progress.Stages[stage]; done {
				continue
			}
			seconds := time.Since(progress.Started).Seconds()
			progress.Stages[stage] = seconds
			if n.PairingMetrics == nil {
				n.PairingMetrics = map[string]*PairingMetric{}
			}
			metric := n.PairingMetrics[stage]
			if metric == nil {
				metric = &PairingMetric{}
				n.PairingMetrics[stage] = metric
			}
			metric.Count++
			metric.Sum += seconds
			for i, bound := range pairingBounds {
				if seconds <= bound {
					metric.Buckets[i]++
				}
			}
			metric.Buckets[6]++
			c.emitLocked(Event{RunID: r.ID, NodeID: n.ID, Type: "pairing.stage", CommandID: progress.CommandID, Data: map[string]any{"peer": peerID, "stage": stage, "elapsedSeconds": seconds, "source": "native-snapshot"}})
		}
	}
}
func pairingBucketLabel(i int) string {
	if i == len(pairingBounds) {
		return "+Inf"
	}
	return fmt.Sprint(pairingBounds[i])
}
