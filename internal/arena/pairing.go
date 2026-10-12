package arena

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PairingCredential is initial provisioning, never live telemetry.
type PairingCredential struct {
	ShipID      string `json:"shipId"`
	Fingerprint string `json:"fingerprint"`
	Secret      string `json:"secret,omitempty"`
}

// StressProfile describes a bounded, versioned multicast workload.
type StressProfile struct {
	Family          string `json:"family,omitempty"`
	ForID           string `json:"forId,omitempty"`
	ForPar          string `json:"forPar,omitempty"`
	ReplayID        string `json:"replayId,omitempty"`
	Version         int    `json:"version"`
	Seed            int64  `json:"seed"`
	Services        int    `json:"services"`
	Rate            int    `json:"rate"`
	Burst           int    `json:"burst"`
	Churn           int    `json:"churn"`
	TTL             int    `json:"ttl"`
	DurationSeconds int    `json:"durationSeconds"`
	Pattern         string `json:"pattern"`
	Service         string `json:"service"`
}

func (p *StressProfile) Validate() error {
	if !Contains([]string{"", "ipv4", "ipv6", "dual-stack"}, p.Family) {
		return errors.New("mDNS family must be ipv4, ipv6 or dual-stack")
	}
	if len(p.ForID) > 128 || len(p.ForPar) > 64 || len(p.ReplayID) > 64 {
		return errors.New("stress target fields exceed bounds")
	}
	if p.Version != 1 || p.Services < 1 || p.Services > 10000 || p.Rate < 1 || p.Rate > 10000 || p.Burst < 1 || p.Burst > 1000 || p.Churn < 0 || p.Churn > 100 || p.TTL < 0 || p.TTL > 86400 || p.DurationSeconds < 1 || p.DurationSeconds > 86400 {
		return errors.New("stress profile v1 needs services/rate 1–10000, burst 1–1000, churn 0–100%, TTL 0–86400 and duration 1–86400")
	}
	if !Contains([]string{"advertise", "query", "churn", "goodbye", "stale", "duplicate", "invalid", "replay"}, p.Pattern) || !Contains([]string{"_ship._tcp", "_shippairing._tcp", "_http._tcp"}, p.Service) {
		return errors.New("unknown mDNS pattern or service")
	}
	return nil
}

func provisionPairing(b *Bundle, nodes []*Node) error {
	if b.Pairing == nil {
		b.Pairing = map[string]PairingCredential{}
	}
	expected := map[string]bool{}
	for _, n := range nodes {
		if n.Role == "STRESSER" {
			continue
		}
		block, _ := pem.Decode([]byte(b.Identities[n.ID].Certificate))
		if block == nil {
			return errors.New("missing certificate")
		}
		fingerprint := sha256.Sum256(block.Bytes)
		n.ShipID = "Arena-" + n.ID
		// The existing C demo uses the node ID as its native SHIP ID.
		if n.Stack == "openeebus" {
			n.ShipID = n.ID
		}
		n.Fingerprint = strings.ToUpper(hex.EncodeToString(fingerprint[:]))
		if len(n.PairingPeers) == 0 {
			continue
		}
		expected[n.ID] = true
		cred, ok := b.Pairing[n.ID]
		if !ok {
			cred = PairingCredential{ShipID: n.ShipID, Fingerprint: n.Fingerprint}
			if n.Role == "CS" {
				secret := make([]byte, 16)
				if _, err := rand.Read(secret); err != nil {
					return err
				}
				cred.Secret = hex.EncodeToString(secret)
			}
		}
		if cred.ShipID != n.ShipID || cred.Fingerprint != n.Fingerprint {
			return fmt.Errorf("pairing identity does not match certificate/node %s", n.ID)
		}
		if n.Role == "CS" {
			secret, err := hex.DecodeString(cred.Secret)
			if err != nil || len(secret) != 16 {
				return fmt.Errorf("listener %s requires a 16-byte hex secret", n.ID)
			}
		} else if cred.Secret != "" {
			return errors.New("announcer must not have a listener secret")
		}
		b.Pairing[n.ID] = cred
	}
	for id := range b.Pairing {
		if !expected[id] {
			return fmt.Errorf("pairing credential %s has no pairing node", id)
		}
	}
	return nil
}

func observedTrust(n *Node, peer *Node) bool {
	if Contains(n.TrustedSKIs, peer.SKI) {
		return true
	}
	trust, _ := nested(n.Snapshot, "pairing", "trusted").(map[string]any)
	for _, v := range trust {
		identity, _ := v.(map[string]any)
		if identity != nil && (identity["ski"] == peer.SKI || strings.EqualFold(fmt.Sprint(identity["fingerprint"]), peer.Fingerprint)) {
			return true
		}
	}
	return false
}

// ResetPairing clears learned state only while the owned node is stopped.
func (c *Controller) ResetPairing(ctx context.Context, runID, nodeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, n, err := c.nodeLocked(runID, nodeID)
	if err != nil {
		return err
	}
	if c.closed || c.scenarios[runID] != nil || !Contains([]string{"created", "stopped", "exited", "removed"}, n.Lifecycle) || Contains([]string{"starting", "stopping", "testing", "editing"}, r.Status) {
		return errors.New("stop the node and scenario before resetting pairing")
	}
	if len(n.PairingPeers) == 0 {
		return errors.New("node has no pairing links")
	}
	if n.Container != "" && c.docker != nil {
		if err = c.docker.Owned(ctx, n.Container, runID, nodeID); err != nil {
			return err
		}
		var info struct{ State struct{ Running bool } }
		if err = c.docker.JSON(ctx, "GET", "/containers/"+n.Container+"/json", nil, &info); err != nil {
			return err
		}
		if info.State.Running {
			return errors.New("container is still running")
		}
	}
	path := c.path(runID, filepath.Join("state", nodeID, "pairing.json"))
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	resetObservation(n)
	c.emitLocked(Event{RunID: runID, NodeID: nodeID, Type: "pairing.reset"})
	c.saveLocked(r)
	return nil
}

func (c *Controller) pairingTarget(runID string, peer *Node) (map[string]any, error) {
	data, err := os.ReadFile(c.path(runID, "bundle.json"))
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err = json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	cred, ok := b.Pairing[peer.ID]
	if !ok {
		return nil, errors.New("pairing provisioning unavailable")
	}
	return map[string]any{"shipId": cred.ShipID, "fingerprint": cred.Fingerprint, "secret": cred.Secret, "ski": peer.SKI}, nil
}
