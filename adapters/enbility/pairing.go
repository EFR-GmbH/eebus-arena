package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	ship "github.com/enbility/ship-go/api"
)

type pairingStore struct {
	mu      sync.Mutex
	path    string
	Trusted map[string]ship.ServiceIdentity `json:"trusted"`
	Entries []ship.DigestEntry              `json:"entries"`
	Next    int                             `json:"next"`
	Version int                             `json:"version"`
	failure string
}

func openPairingStore(dir string) (*pairingStore, error) {
	p := &pairingStore{
		path:    filepath.Join(dir, "pairing.json"),
		Trusted: map[string]ship.ServiceIdentity{},
		Version: 1,
	}
	data, err := os.ReadFile(p.path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	// Existing files must explicitly contain their version and trust map.
	p.Version = 0
	p.Trusted = nil
	if err = json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("corrupt pairing state: %w", err)
	}
	if p.Version != 1 || p.Trusted == nil || p.Next < 0 ||
		(len(p.Entries) == 0 && p.Next != 0) ||
		(len(p.Entries) > 0 && p.Next >= len(p.Entries)) {
		return nil, errors.New("invalid pairing state")
	}
	for _, identity := range p.Trusted {
		if _, err := identity.ToServiceDetails(); err != nil {
			return nil, fmt.Errorf("invalid persisted identity: %w", err)
		}
	}
	return p, nil
}

// Persist the replay buffer and trust as one document. Sync the temporary file
// before rename and the directory afterwards so a crash cannot roll back trust
// without also rolling back its replay history. The caller holds p.mu.
func (p *pairingStore) saveLocked() error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p.path), ".pairing-")
	if err != nil {
		p.failure = err.Error()
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, p.path)
	}
	if err == nil {
		var d *os.File
		d, err = os.Open(filepath.Dir(p.path))
		if err == nil {
			err = d.Sync()
			d.Close()
		}
	}
	if err != nil {
		p.failure = err.Error()
	}
	return err
}

func (p *pairingStore) LoadRingBuffer() ([]ship.DigestEntry, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ship.DigestEntry(nil), p.Entries...), p.Next, nil
}

func (p *pairingStore) SaveRingBuffer(entries []ship.DigestEntry, next int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Entries = append([]ship.DigestEntry(nil), entries...)
	p.Next = next
	return p.saveLocked()
}

func (p *pairingStore) update(i ship.ServiceIdentity, remove bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := i.Fingerprint
	if key == "" {
		key = i.SKI
	}
	for k, v := range p.Trusted {
		if (i.ShipID != "" && v.ShipID == i.ShipID) || (i.SKI != "" && v.SKI == i.SKI) {
			delete(p.Trusted, k)
		}
	}
	if !remove {
		p.Trusted[key] = i
	}
	return p.saveLocked()
}

func (p *pairingStore) identities() []ship.ServiceIdentity {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []ship.ServiceIdentity{}
	for _, i := range p.Trusted {
		out = append(out, i)
	}
	return out
}

func (a *adapter) pairingSnapshot() map[string]any {
	if a.pairing == nil {
		return nil
	}
	trusted := map[string]ship.ServiceIdentity{}
	for _, i := range a.pairing.identities() {
		trusted[i.Fingerprint] = i
	}
	a.pairing.mu.Lock()
	failure := a.pairing.failure
	a.pairing.mu.Unlock()
	a.traceMu.Lock()
	totals := map[string]uint64{}
	for k, v := range a.traceTotals {
		totals[k] = v
	}
	a.traceMu.Unlock()
	return map[string]any{
		"counters":         totals,
		"role":             a.cfg.PairingRole,
		"trusted":          trusted,
		"announcements":    a.service.GetActiveAnnouncements(),
		"listener":         a.service.ArenaListenerStatus(),
		"persistenceError": failure,
	}
}

func (a *adapter) persistIdentity(i ship.ServiceIdentity, remove bool) {
	if a.pairing == nil {
		return
	}
	if err := a.pairing.update(i, remove); err != nil {
		a.emit("pairing.persistence", "", map[string]any{"ok": false, "error": err.Error()})
	} else {
		a.emit("pairing.persistence", "", map[string]any{"ok": true})
	}
}

func (a *adapter) handlePairing(c command) {
	if a.pairing == nil {
		a.result(c, errors.New("pairing not configured"), nil)
		return
	}
	switch c.Command {
	case "pairing.status":
		a.result(c, nil, a.pairingSnapshot())
	case "pairing.announce":
		target, ok := c.Params["target"].(map[string]any)
		if !ok {
			a.result(c, errors.New("missing target"), nil)
			return
		}
		secret, err := hex.DecodeString(fmt.Sprint(target["secret"]))
		if err != nil || len(secret) != 16 {
			a.result(c, errors.New("invalid listener secret"), nil)
			return
		}
		i := ship.NewServiceIdentity("", fmt.Sprint(target["fingerprint"]), fmt.Sprint(target["shipId"]))
		a.service.RegisterRemoteService(i)
		err = a.service.StartAnnouncementTo(ship.PairingTarget{
			ShipID:      i.ShipID,
			Fingerprint: i.Fingerprint,
			Secret:      secret,
		})
		if err == nil {
			err = a.pairing.update(i, false)
			a.emit("pairing.announcement", c.ID, map[string]any{"shipId": i.ShipID, "active": true})
		}
		a.result(c, err, nil)
	case "pairing.stop":
		id, ok := c.Params["shipId"].(string)
		if !ok || id == "" {
			a.result(c, errors.New("missing shipId"), nil)
			return
		}
		err := a.service.StopAnnouncementTo(id)
		if err == nil {
			a.emit("pairing.announcement", c.ID, map[string]any{"shipId": id, "active": false})
		}
		a.result(c, err, nil)
	}
}

// trace bounds repetitive observations; native decisions and trust stay native.
func (a *adapter) trace(kind string, data map[string]any) {
	a.traceMu.Lock()
	if a.traceTotals == nil {
		a.traceTotals = map[string]uint64{}
	}
	key := kind
	if stage, ok := data["stage"].(string); ok {
		key += "." + stage
	}
	a.traceTotals[key]++
	if kind == "ship.transport" || kind == "pairing.listener" || kind == "pairing.replacement" {
		a.traceMu.Unlock()
		a.emit(kind, "", data)
		return
	}
	now := time.Now()
	if now.Sub(a.traceWindow) >= time.Second {
		if a.traceSuppressed > 0 {
			a.emit("pairing.suppressed", "", map[string]any{"count": a.traceSuppressed})
		}
		a.traceWindow = now
		a.traceCount = 0
		a.traceSuppressed = 0
	}
	if a.traceCount >= 100 {
		a.traceSuppressed++
		if a.traceSuppressed == 1 {
			a.emit("pairing.suppressed", "", map[string]any{"count": 1, "boundary": "started"})
		}
		a.traceTotals["suppressed"]++
		a.traceMu.Unlock()
		return
	}
	a.traceCount++
	a.traceMu.Unlock()
	a.emit(kind, "", data)
}
