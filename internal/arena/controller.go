package arena

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Controller struct {
	mu             sync.Mutex
	mutationMu     sync.Mutex
	mutations      map[string]chan struct{}
	data, hostData string
	docker         *Docker
	variants       map[string]Variant
	runs           map[string]*Run
	journal        map[string]*os.File
	events         []Event
	index          uint64
	epoch          string
	closed         bool
	workers        sync.WaitGroup
	saveMu         sync.Mutex
	pendingSaves   map[string][]byte
	saveSignal     chan struct{}
	saveStop       chan struct{}
	saveWorkers    sync.WaitGroup
	persist        func(string, []byte) error
	catalogErrors  []string
	subscribers    map[chan Event]bool
	pending        map[string]pendingCommand
	watchers       map[string]context.CancelFunc
	scenarios      map[string]context.CancelFunc
	dirty          map[string]bool
	controlMetrics map[string]any
	resources      *resourceStore
	faults         map[string]*FaultLease
	tester         map[string]any
	starts         map[string]startOperation
	ctx            context.Context
	cancel         context.CancelFunc
}

type pendingCommand struct {
	run, node, incarnation string
	response               chan Event
}
type startOperation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func id() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeJSONBytes(path, append(b, '\n'))
}
func writeJSONBytes(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".arena-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func NewController(data, hostData string, docker *Docker) (*Controller, error) {
	if hostData == "" {
		hostData = data
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{data: data, hostData: hostData, docker: docker, variants: map[string]Variant{}, runs: map[string]*Run{}, journal: map[string]*os.File{}, subscribers: map[chan Event]bool{}, pending: map[string]pendingCommand{}, watchers: map[string]context.CancelFunc{}, scenarios: map[string]context.CancelFunc{}, dirty: map[string]bool{}, ctx: ctx, cancel: cancel}
	c.pendingSaves = map[string][]byte{}
	c.saveSignal, c.saveStop = make(chan struct{}, 1), make(chan struct{})
	c.persist = writeJSONBytes
	c.starts = map[string]startOperation{}
	c.epoch = id()
	c.tester = testerFingerprint(docker)
	if err := os.MkdirAll(filepath.Join(data, "runs"), 0700); err != nil {
		return nil, err
	}
	c.resources = newResourceStore(data)
	ready := false
	defer func() {
		if !ready {
			c.resources.close()
			for _, file := range c.journal {
				file.Close()
			}
		}
	}()
	if err := c.loadVariants(false); err != nil {
		c.catalogErrors = append(c.catalogErrors, err.Error())
	}
	entries, _ := os.ReadDir(filepath.Join(data, "runs"))
	for _, entry := range entries {
		if !entry.IsDir() || !safeID.MatchString(entry.Name()) {
			continue
		}
		var run Run
		err := recoverEdit(c.path(entry.Name(), ""))
		var b []byte
		if err == nil {
			b, err = os.ReadFile(c.path(entry.Name(), "run.json"))
		}
		if err == nil {
			err = json.Unmarshal(b, &run)
		}
		if err == nil && run.ID != entry.Name() {
			err = errors.New("run ID does not match its directory")
		}
		if err != nil {
			log.Printf("loading arena %s: %v", entry.Name(), err)
			c.runs[entry.Name()] = &Run{ID: entry.Name(), Definition: Definition{Name: "Unreadable arena"}, Status: "corrupt", Error: "loading run: " + err.Error(), Nodes: []*Node{}, Assertions: []Assertion{}}
			continue
		}
		if run.Status == "starting" || run.Status == "testing" || run.Status == "stopping" || run.Status == "dropping" {
			run.Status = "interrupted"
			run.Assertions = append(run.Assertions, Assertion{ID: "interrupted", Outcome: "inconclusive", Detail: "orchestrator exited during a run", At: time.Now().UTC()})
		}
		c.runs[run.ID] = &run
		if err := c.openJournal(run.ID); err != nil {
			return nil, err
		}
		c.saveLocked(&run)
	}
	c.saveWorkers.Add(1)
	go func() { defer c.saveWorkers.Done(); c.persistLoop() }()
	c.workers.Add(1)
	go func() { defer c.workers.Done(); c.poll() }()
	if c.docker != nil {
		// Complete orphan cleanup before exposing the API, so it cannot race
		// with a new address inspection started by this controller.
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		if err := c.cleanupAddressInspections(ctx); err != nil {
			c.Emit(Event{Type: "fault.inspection_cleanup_unverified", Data: map[string]any{"error": err.Error()}})
		}
		cancel()
		c.restoreFaultWatchers()
	}
	ready = true
	return c, nil
}
func (c *Controller) path(runID, name string) string {
	return filepath.Join(c.data, "runs", runID, name)
}
func (c *Controller) openJournal(runID string) error {
	f, err := os.OpenFile(c.path(runID, "events.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		c.journal[runID] = f
	}
	return err
}
func (c *Controller) LoadVariants() error {
	return c.loadVariants(true)
}
func (c *Controller) loadVariants(strict bool) error {
	b, err := os.ReadFile(filepath.Join(c.data, "variants.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var variants []Variant
	if err = json.Unmarshal(b, &variants); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	updated := make(map[string]Variant, len(c.variants)+len(variants))
	for k, v := range c.variants {
		updated[k] = v
	}
	for _, v := range variants {
		if !safeID.MatchString(v.ID) || !immutableImageID.MatchString(v.Image) {
			err := fmt.Errorf("variant %s must have a safe id and immutable image ID", v.ID)
			if strict {
				return err
			}
			c.catalogErrors = append(c.catalogErrors, err.Error())
			continue
		}
		if old, ok := updated[v.ID]; ok && old.Image != v.Image {
			err := fmt.Errorf("variant %s is immutable", v.ID)
			if strict {
				return err
			}
			c.catalogErrors = append(c.catalogErrors, err.Error())
			continue
		}
		updated[v.ID] = v
	}
	c.variants = updated
	if strict {
		c.catalogErrors = nil
	}
	return nil
}
func (c *Controller) saveLocked(r *Run) {
	if r == nil || r.Status == "corrupt" || r.Status == "dropping" || r.Status == "editing" {
		return
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		r.Error = "serializing run: " + err.Error()
		c.dirty[r.ID] = true
		return
	}
	c.pendingSaves[r.ID] = append(body, '\n')
	delete(c.dirty, r.ID)
	select {
	case c.saveSignal <- struct{}{}:
	default:
	}
}
func (c *Controller) persistLoop() {
	for {
		select {
		case <-c.saveStop:
			return
		case <-c.saveSignal:
			c.flushSaves()
		}
	}
}

// Preserve write order while keeping disk writes and syncs outside the state lock.
func (c *Controller) flushSaves() {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	for {
		c.mu.Lock()
		var runID string
		var body []byte
		for key, value := range c.pendingSaves {
			runID, body = key, value
			break
		}
		if body == nil {
			c.mu.Unlock()
			return
		}
		delete(c.pendingSaves, runID)
		path, persist := c.path(runID, "run.json"), c.persist
		c.mu.Unlock()
		if err := persist(path, body); err != nil {
			c.mu.Lock()
			if r := c.runs[runID]; r != nil {
				r.Error = "persisting run: " + err.Error()
				c.dirty[runID] = true
			}
			c.mu.Unlock()
		}
	}
}
func (c *Controller) emitLocked(e Event) {
	e.ReceivedAt = time.Now().UTC()
	if r := c.runs[e.RunID]; r != nil {
		origin, _ := r.Runtime["clockOrigin"].(string)
		if at, err := time.Parse(time.RFC3339Nano, origin); err == nil {
			elapsed := e.ReceivedAt.Sub(at).Seconds() * float64(r.Definition.TimeScale)
			e.ProtocolSeconds = &elapsed
		}
	}
	c.index++
	e.Index = c.index
	e.Epoch = c.epoch
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	// Node snapshot maps change later. Copy the data so the journal, replay and
	// SSE all keep the event as it was published.
	if e.Data != nil {
		encoded, err := json.Marshal(e.Data)
		if err != nil {
			return
		}
		var immutable map[string]any
		if json.Unmarshal(encoded, &immutable) != nil {
			return
		}
		e.Data = immutable
	}
	if _, n, err := c.nodeLocked(e.RunID, e.NodeID); err == nil && e.Type != "metrics" && e.Type != "snapshot" && e.Type != "log" {
		if n.Counters == nil {
			n.Counters = map[string]uint64{}
		}
		n.Counters[e.Type]++
	}
	c.events = append(c.events, e)
	if len(c.events) > 4000 {
		c.events = c.events[len(c.events)-4000:]
	}
	if f := c.journal[e.RunID]; f != nil {
		if info, err := f.Stat(); err == nil && info.Size() > 64<<20 {
			f.Close()
			os.Remove(c.path(e.RunID, "events.ndjson.2"))
			os.Rename(c.path(e.RunID, "events.ndjson.1"), c.path(e.RunID, "events.ndjson.2"))
			os.Rename(c.path(e.RunID, "events.ndjson"), c.path(e.RunID, "events.ndjson.1"))
			c.openJournal(e.RunID)
			f = c.journal[e.RunID]
			if f != nil {
				json.NewEncoder(f).Encode(Event{RunID: e.RunID, At: time.Now().UTC(), Type: "evidence.rotated", Data: map[string]any{"retainedFiles": 3, "maxFileBytes": 64 << 20}})
			}
		}
		if f != nil {
			if err := json.NewEncoder(f).Encode(e); err != nil {
				if r := c.runs[e.RunID]; r != nil {
					r.Error = "writing evidence: " + err.Error()
				}
			}
		}
	}
	for ch := range c.subscribers {
		select {
		case ch <- e:
		default:
			close(ch)
			delete(c.subscribers, ch)
		}
	}
	if e.Type == "command.result" {
		if p, exists := c.pending[e.CommandID]; exists && p.run == e.RunID && p.node == e.NodeID && p.incarnation == e.Incarnation {
			select {
			case p.response <- e:
			default:
			}
		}
	}
}
func (c *Controller) Emit(e Event) { c.mu.Lock(); defer c.mu.Unlock(); c.emitLocked(e) }
func (c *Controller) Subscribe(after uint64) (chan Event, []Event) {
	return c.subscribe("", after)
}
func (c *Controller) subscribe(epoch string, after uint64) (chan Event, []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan Event, 4096)
	c.subscribers[ch] = true
	history := []Event{}
	if after > c.index || epoch != "" && epoch != c.epoch {
		reason := "controller restarted; inspect saved evidence"
		if epoch == "legacy" {
			reason = "legacy cursor lacks controller epoch; replaying retained history"
		}
		history = append(history, Event{Epoch: c.epoch, At: time.Now().UTC(), Type: "telemetry.gap", Data: map[string]any{"reason": reason}})
		after = 0
	}
	if len(c.events) > 0 && after > 0 && after < c.events[0].Index {
		history = append(history, Event{Epoch: c.epoch, Index: c.events[0].Index - 1, At: time.Now().UTC(), Type: "telemetry.gap", Data: map[string]any{"reason": "live history expired; inspect saved evidence"}})
	}
	for _, e := range c.events {
		if e.Index > after {
			history = append(history, e)
		}
	}
	return ch, history
}
func (c *Controller) Unsubscribe(ch chan Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subscribers[ch] {
		delete(c.subscribers, ch)
		close(ch)
	}
}
func (c *Controller) Snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"runs": c.runs, "variants": c.variants, "catalogErrors": c.catalogErrors, "cursor": c.index, "epoch": c.epoch, "controlMetrics": c.controlMetrics})
	var out map[string]any
	json.Unmarshal(b, &out)
	// Routine state polling must not distribute device-control credentials.
	if runs, ok := out["runs"].(map[string]any); ok {
		for _, value := range runs {
			run, _ := value.(map[string]any)
			definition, _ := run["definition"].(map[string]any)
			for _, key := range []string{"nodes", "groups"} {
				parent := run
				if key == "groups" {
					parent = definition
				}
				items, _ := parent[key].([]any)
				for _, item := range items {
					node, _ := item.(map[string]any)
					if external, ok := node["external"].(map[string]any); ok {
						delete(external, "token")
					}
				}
			}
		}
	}
	return out
}
func (c *Controller) Create(bundle Bundle) (string, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errors.New("controller is closed")
	}
	variants := make(map[string]Variant, len(c.variants))
	for k, v := range c.variants {
		variants[k] = v
	}
	tester := c.tester
	c.mu.Unlock()
	// An import must match an installed variant. It cannot replace one.
	for _, v := range bundle.Variants {
		known, ok := variants[v.ID]
		if !ok || known.Image != v.Image {
			return "", fmt.Errorf("import needs installed variant %s with image %s", v.ID, v.Image)
		}
	}
	nodes, err := Resolve(&bundle, variants)
	if err != nil {
		return "", err
	}
	runID := "run-" + id()
	if err = os.MkdirAll(c.path(runID, "identities"), 0700); err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.RemoveAll(c.path(runID, ""))
		}
	}()
	if err = writeNodeSetup(c.path(runID, ""), bundle, nodes); err != nil {
		return "", err
	}
	if err = writeJSON(c.path(runID, "bundle.json"), bundle); err != nil {
		return "", err
	}
	run := &Run{ID: runID, Definition: bundle.Definition, Variants: bundle.Variants, Status: "created", Created: time.Now().UTC(), Nodes: nodes, Assertions: []Assertion{}, Runtime: map[string]any{"tester": tester}}
	if err = writeJSON(c.path(runID, "compose.json"), c.compose(run, variants)); err != nil {
		return "", err
	}
	if err = writeJSON(c.path(runID, "run.json"), run); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errors.New("controller is closed")
	}
	if err = c.openJournal(runID); err != nil {
		return "", err
	}
	cleanup = false
	c.runs[runID] = run
	c.emitLocked(Event{RunID: runID, Type: "run.created"})
	return runID, nil
}
func writeNodeSetup(directory string, bundle Bundle, nodes []*Node) error {
	var err error
	for _, n := range nodes {
		p := filepath.Join(directory, "identities", n.ID)
		if err = os.MkdirAll(p, 0700); err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Join(directory, "state", n.ID), 0700); err != nil {
			return err
		}
		identity := bundle.Identities[n.ID]
		identityFiles := map[string]string{}
		if n.Role != "STRESSER" {
			identityFiles = map[string]string{"cert.pem": identity.Certificate, "key.pem": identity.PrivateKey}
		}
		for name, value := range identityFiles {
			if err = os.WriteFile(filepath.Join(p, name), []byte(value), 0600); err != nil {
				return err
			}
		}
		cfg := map[string]any{"schemaVersion": 1, "nodeId": n.ID, "role": n.Role, "trustedSkis": n.TrustedSKIs, "certificate": "/identity/cert.pem", "privateKey": "/identity/key.pem", "heartbeatIntervalSeconds": 60, "timeScale": bundle.Definition.TimeScale, "port": 4712}
		cfg["stateDirectory"] = "/state"
		cfg["resourceIntervalMillis"] = bundle.Definition.samplingInterval(n.ID)
		cfg["shipId"] = n.ShipID
		cfg["networkMode"] = bundle.Definition.networkMode()
		cfg["monitoring"] = n.Monitoring
		if cred, ok := bundle.Pairing[n.ID]; ok {
			cfg["pairing"] = cred
			cfg["pairingRole"] = map[string]string{"EG": "announcer", "CS": "listener"}[n.Role]
		}
		for _, g := range bundle.Definition.Groups {
			if g.ID == n.Group && g.Stress != nil {
				cfg["stress"] = g.Stress
			}
		}
		if err = writeJSON(filepath.Join(p, "config.json"), cfg); err != nil {
			return err
		}
		if n.Role == "STRESSER" {
			// This mount contains no identities or secrets. The worker drops all
			// capabilities, including the root user's DAC override.
			if err = os.Chmod(p, 0755); err != nil {
				return err
			}
			if err = os.Chmod(filepath.Join(p, "config.json"), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *Controller) compose(run *Run, variants map[string]Variant) any {
	services := map[string]any{}
	for _, n := range run.Nodes {
		if n.External != nil {
			continue
		}
		v := variants[n.Variant]
		service := map[string]any{"image": v.Image, "stdin_open": true, "tty": false, "restart": "no", "init": true, "networks": []string{"arena"}, "volumes": []string{filepath.Join(c.hostData, "runs", run.ID, "identities", n.ID) + ":/identity:ro", filepath.Join(c.hostData, "runs", run.ID, "state", n.ID) + ":/state"}, "labels": map[string]string{"org.eebus-arena.run": run.ID, "org.eebus-arena.node": n.ID}, "logging": map[string]any{"driver": "json-file", "options": map[string]string{"max-size": "16m", "max-file": "3"}}}
		if run.Definition.networkMode() == "ipv4" {
			service["sysctls"] = map[string]string{"net.ipv6.conf.all.disable_ipv6": "1", "net.ipv6.conf.default.disable_ipv6": "1"}
		}
		if n.Role == "STRESSER" {
			service["cap_drop"] = []string{"ALL"}
		}
		run.Definition.resourcesFor(n.ID, n.Group).applyCompose(service)
		services[n.ID] = service
	}
	network := c.composeNetwork(run)
	return map[string]any{"name": "eebus-" + run.ID, "services": services, "networks": map[string]any{"arena": network}}
}
func (c *Controller) Start(runID string) error {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil {
		c.mu.Unlock()
		return errors.New("run not found")
	}
	if c.closed || !Contains([]string{"created", "stopped", "failed"}, r.Status) || c.scenarios[runID] != nil {
		c.mu.Unlock()
		return errors.New("run already active")
	}
	if err := c.clockPreflightLocked(r); err != nil {
		c.emitLocked(Event{RunID: runID, Type: "clock.unsupported", Data: map[string]any{"error": err.Error()}})
		c.mu.Unlock()
		return err
	}
	r.Status = "starting"
	r.Runtime["clockOrigin"] = time.Now().UTC().Format(time.RFC3339Nano)
	r.Error = ""
	r.generation++
	for _, n := range r.Nodes {
		c.resetNodeLocked(runID, n, time.Time{})
	}
	ctx, startCancel := context.WithTimeout(c.ctx, 5*time.Minute)
	op := startOperation{cancel: startCancel, done: make(chan struct{})}
	c.starts[runID] = op
	c.saveLocked(r)
	c.workers.Add(1)
	c.mu.Unlock()
	c.flushSaves()
	go func() {
		defer c.workers.Done()
		defer func() { startCancel(); c.mu.Lock(); delete(c.starts, runID); close(op.done); c.mu.Unlock() }()
		var out []byte
		var err error
		if c.docker != nil {
			err = c.checkProjectOwnership(ctx, r)
		}
		if err == nil {
			err = c.startPhysical(ctx, r)
		}
		if err == nil {
			cmd := exec.CommandContext(ctx, "docker", "compose", "-f", c.path(runID, "compose.json"), "up", "-d", "--force-recreate", "--pull", "never")
			out, err = cmd.CombinedOutput()
		}
		c.Emit(Event{RunID: runID, Type: "runtime.compose", Data: map[string]any{"output": string(out)}})
		if err != nil {
			cleanup, cancel := context.WithTimeout(c.ctx, 15*time.Second)
			if releaseErr := c.stopPhysical(cleanup, r); releaseErr != nil {
				c.Emit(Event{RunID: runID, Type: "external.cleanup_failed", Data: map[string]any{"error": releaseErr.Error()}})
			}
			cancel()
		}
		c.mu.Lock()
		r := c.runs[runID]
		stopping := r.Status == "stopping"
		switch {
		case stopping:
			// Down waits for this goroutine, then removes the services.
		case c.closed:
			r.Status = "interrupted"
			r.Error = "controller shut down during startup"
		case err != nil:
			r.Status = "failed"
			r.Error = err.Error() + ": " + string(out)
		default:
			r.Status = "running"
		}
		c.saveLocked(r)
		c.mu.Unlock()
		if err == nil && !stopping && ctx.Err() == nil {
			c.reconcile(runID)
			if r.Definition.Scenario != "manual" {
				if err := c.Test(runID); err != nil {
					c.assert(runID, "", "scenario-start", "inconclusive", err.Error(), "")
				}
			}
		}
	}()
	return nil
}
func (c *Controller) Down(runID string) (err error) {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil {
		c.mu.Unlock()
		return errors.New("run not found")
	}
	if c.closed || r.Status == "dropping" || r.Status == "editing" || r.Status == "corrupt" || r.Status == "stopping" || r.Status == "archived" {
		c.mu.Unlock()
		return errors.New("arena cannot be stopped in its current state")
	}
	if cancel := c.scenarios[runID]; cancel != nil {
		cancel()
	}
	op, starting := c.starts[runID]
	if starting {
		op.cancel()
	}
	c.workers.Add(1)
	defer c.workers.Done()
	r.Status = "stopping"
	r.generation++
	for _, n := range r.Nodes {
		c.stopWatcherLocked(runID, n.ID)
	}
	c.saveLocked(r)
	c.mu.Unlock()
	containersRemoved := false
	defer func() {
		if err != nil && !containersRemoved {
			c.mu.Lock()
			r.Status, r.Error = "failed", "stopping arena: "+err.Error()
			if c.closed {
				r.Status = "interrupted"
			}
			c.saveLocked(r)
			c.mu.Unlock()
			c.flushSaves()
		}
	}()
	// Check the ownership of every container before compose down removes it.
	ctx, cancel := context.WithTimeout(c.ctx, 90*time.Second)
	defer cancel()
	if starting {
		select {
		case <-op.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.docker == nil {
		return errors.New("Docker is unavailable")
	}
	physicalErr := c.stopPhysical(ctx, r)
	faultErr := c.restoreRunFaults(ctx, runID)
	if err = c.checkProjectOwnership(ctx, r); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", c.path(runID, "compose.json"), "down", "--timeout", "10")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compose down: %w: %s", err, out)
	}
	containersRemoved = true
	c.mu.Lock()
	defer func() { c.mu.Unlock(); c.flushSaves() }()
	r.Status = "stopped"
	for _, n := range r.Nodes {
		lastProcesses, lastLimits := n.ProcessMetrics, n.EffectiveResources
		n.Lifecycle = "removed"
		if n.External != nil && physicalErr != nil {
			n.Lifecycle = "unavailable"
		}
		n.Container = ""
		c.resetNodeLocked(runID, n, time.Time{})
		n.ProcessMetrics, n.EffectiveResources = lastProcesses, lastLimits
	}
	c.saveLocked(r)
	cleanupErr := errors.Join(physicalErr, faultErr)
	if cleanupErr != nil {
		r.Error = "containers removed; cleanup incomplete: " + cleanupErr.Error()
		c.saveLocked(r)
		c.emitLocked(Event{RunID: runID, Type: "run.cleanup.failed", Data: map[string]any{"error": cleanupErr.Error(), "containersRemoved": true}})
	} else {
		r.Error = ""
		c.saveLocked(r)
		c.emitLocked(Event{RunID: runID, Type: "run.stopped"})
	}
	return cleanupErr
}

// Compose recreation and removal must only affect this arena's labelled nodes.
func (c *Controller) checkProjectOwnership(ctx context.Context, r *Run) error {
	containers, err := c.docker.ProjectContainers(ctx, r.ID)
	if err != nil {
		return fmt.Errorf("checking arena containers: %w", err)
	}
	for _, container := range containers {
		labels, _ := container["Labels"].(map[string]any)
		nodeID := fmt.Sprint(labels["org.eebus-arena.node"])
		valid := false
		for _, n := range r.Nodes {
			if n.ID == nodeID {
				valid = true
				break
			}
		}
		if !valid {
			return errors.New("unexpected container in arena project; lifecycle change aborted")
		}
		if err := c.docker.Owned(ctx, fmt.Sprint(container["Id"]), r.ID, nodeID); err != nil {
			return err
		}
	}
	return nil
}
func (c *Controller) nodeLocked(runID, nodeID string) (*Run, *Node, error) {
	r := c.runs[runID]
	if r == nil {
		return nil, nil, errors.New("run not found")
	}
	for _, n := range r.Nodes {
		if n.ID == nodeID {
			return r, n, nil
		}
	}
	return nil, nil, errors.New("node not found")
}
func (c *Controller) Action(ctx context.Context, runID, nodeID, action string) error {
	release, err := c.mutation(ctx, runID, nodeID)
	if err != nil {
		return err
	}
	defer release()
	c.mu.Lock()
	r, n, err := c.nodeLocked(runID, nodeID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if c.closed || !reconcilable(r.Status) {
		c.mu.Unlock()
		return errors.New("arena is not active")
	}
	container := n.Container
	if n.External != nil {
		external := *n.External
		c.workers.Add(1)
		c.mu.Unlock()
		defer c.workers.Done()
		return c.physicalAction(ctx, runID, nodeID, action, &external)
	}
	if c.docker == nil || container == "" {
		c.mu.Unlock()
		return errors.New("node has no container")
	}
	c.workers.Add(1)
	c.mu.Unlock()
	defer c.workers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	if err = c.docker.Owned(ctx, container, runID, nodeID); err != nil {
		return err
	}
	c.Emit(Event{RunID: runID, NodeID: nodeID, Type: "action.lifecycle", Data: map[string]any{"action": action}})
	issued := time.Now().UTC()
	var previous Node
	var actionGeneration uint64
	resets := Contains([]string{"start", "restart", "stop", "kill"}, action)
	c.mu.Lock()
	r, n, lookupErr := c.nodeLocked(runID, nodeID)
	if lookupErr != nil || c.closed || !reconcilable(r.Status) || n.Container != container {
		c.mu.Unlock()
		return errors.New("node lifecycle changed")
	}
	r.generation++
	n.action++
	actionGeneration = n.action
	if resets {
		encoded, _ := json.Marshal(n)
		json.Unmarshal(encoded, &previous)
		c.resetNodeLocked(runID, n, issued)
		n.Lifecycle = "changing"
		c.dirty[runID] = true
	}
	c.mu.Unlock()
	err = c.docker.Action(ctx, container, action)
	if err == nil && resets {
		c.mu.Lock()
		r, n, lookupErr := c.nodeLocked(runID, nodeID)
		if lookupErr == nil && !c.closed && n.Container == container && n.action == actionGeneration {
			n.Lifecycle = "starting"
			if action == "stop" || action == "kill" {
				n.Lifecycle = "exited"
			}
			c.dirty[r.ID] = true
		}
		c.mu.Unlock()
	}
	if err != nil && resets {
		c.mu.Lock()
		r, n, lookupErr := c.nodeLocked(runID, nodeID)
		if lookupErr == nil && !c.closed && reconcilable(r.Status) && n.Container == container && n.action == actionGeneration && n.Incarnation == "" {
			c.stopWatcherLocked(runID, nodeID)
			*n = previous
			n.action = actionGeneration
			r.generation++
			c.dirty[runID] = true
		}
		c.mu.Unlock()
	}
	c.reconcileNode(ctx, runID, nodeID, container)
	return err
}
func (c *Controller) Command(ctx context.Context, runID, nodeID string, command Command) (Event, error) {
	release, err := c.mutation(ctx, runID, nodeID)
	if err != nil {
		return Event{}, err
	}
	defer release()
	// Copy the params: scenario steps reuse the same map for every node.
	params := make(map[string]any, len(command.Params))
	for k, v := range command.Params {
		params[k] = v
	}
	command.Params = params
	c.mu.Lock()
	_, n, err := c.nodeLocked(runID, nodeID)
	if err != nil {
		c.mu.Unlock()
		return Event{}, err
	}
	if c.closed {
		c.mu.Unlock()
		return Event{}, errors.New("controller is closed")
	}
	if !Contains(n.Capabilities, command.Command) {
		c.mu.Unlock()
		return Event{}, fmt.Errorf("unsupported capability: %s", command.Command)
	}
	if strings.HasPrefix(command.Command, "pairing.") && command.Command != "pairing.status" {
		target, ok := command.Params["targetNode"].(string)
		if !ok || target == "" {
			c.mu.Unlock()
			return Event{}, errors.New("pairing command requires a targetNode string")
		}
	}
	if target, ok := command.Params["targetNode"].(string); ok {
		_, partner, err := c.nodeLocked(runID, target)
		if err != nil {
			c.mu.Unlock()
			return Event{}, err
		}
		if strings.HasPrefix(command.Command, "pairing.") {
			if n.Role != "EG" || !Contains(n.PairingPeers, partner.ID) {
				c.mu.Unlock()
				return Event{}, errors.New("targetNode must be an intended pairing listener")
			}
			if command.Command == "pairing.announce" {
				target, err := c.pairingTarget(runID, partner)
				if err != nil {
					c.mu.Unlock()
					return Event{}, err
				}
				fault, _ := command.Params["credentialFault"].(string)
				switch fault {
				case "":
				case "wrong-secret":
					target["secret"] = strings.Repeat("0", 32)
				case "wrong-target-id":
					target["shipId"] = fmt.Sprint(target["shipId"]) + "-mismatch"
				case "wrong-fingerprint":
					target["fingerprint"] = strings.Repeat("0", 64)
				default:
					c.mu.Unlock()
					return Event{}, errors.New("unknown credentialFault")
				}
				command.Params = map[string]any{"target": target, "targetNode": partner.ID, "credentialFault": fault}
			} else {
				command.Params = map[string]any{"shipId": partner.ShipID, "targetNode": partner.ID}
			}
		} else {
			if !observedTrust(n, partner) {
				c.mu.Unlock()
				return Event{}, errors.New("targetNode must be a trusted partner")
			}
			command.Params["targetSki"] = partner.SKI
			delete(command.Params, "targetNode")
		}
	}
	container := n.Container
	command.ID = id()
	if command.Command == "pairing.announce" {
		peerID, _ := command.Params["targetNode"].(string)
		_, peer, _ := c.nodeLocked(runID, peerID)
		if peer != nil {
			now := time.Now().UTC()
			beginPairing(n, peerID, command.ID, now)
			beginPairing(peer, n.ID, command.ID, now)
		}
	}
	response := make(chan Event, 1)
	external := n.External
	if c.docker == nil && external == nil {
		c.mu.Unlock()
		return Event{}, errors.New("Docker is unavailable")
	}
	c.workers.Add(1)
	c.pending[command.ID] = pendingCommand{run: runID, node: nodeID, incarnation: n.Incarnation, response: response}
	c.mu.Unlock()
	defer c.workers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	defer func() { c.mu.Lock(); delete(c.pending, command.ID); c.mu.Unlock() }()
	if external == nil {
		err = c.docker.Owned(ctx, container, runID, nodeID)
	}
	if err != nil {
		return Event{}, err
	}
	evidenceParams := command.Params
	if strings.HasPrefix(command.Command, "pairing.") {
		evidenceParams = map[string]any{"targetNode": command.Params["targetNode"], "credentialFault": command.Params["credentialFault"]}
	}
	c.Emit(Event{RunID: runID, NodeID: nodeID, Type: "action.command", CommandID: command.ID, Data: map[string]any{"command": command.Command, "params": evidenceParams}})
	if external != nil {
		err = externalRequest(ctx, external, "POST", "/command", runID, nodeID, command, nil)
	} else {
		err = c.docker.Send(ctx, container, command)
	}
	if err != nil {
		return Event{}, err
	}
	select {
	case e := <-response:
		return e, nil
	case <-ctx.Done():
		c.Emit(Event{RunID: runID, NodeID: nodeID, Type: "adapter.command_timeout", CommandID: command.ID})
		return Event{}, ctx.Err()
	}
}
func (c *Controller) poll() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			for id := range c.dirty {
				c.saveLocked(c.runs[id])
			}
			ids := []string{}
			for k, r := range c.runs {
				if reconcilable(r.Status) {
					ids = append(ids, k)
				}
			}
			c.mu.Unlock()
			for _, runID := range ids {
				c.reconcile(runID)
			}
			c.sampleControl()
		}
	}
}
func reconcilable(status string) bool {
	return Contains([]string{"running", "testing", "complete", "interrupted", "failed"}, status)
}
func (c *Controller) stopWatcherLocked(runID, nodeID string) {
	key := runID + "/" + nodeID
	if stop := c.watchers[key]; stop != nil {
		stop()
		delete(c.watchers, key)
	}
}
func resetObservation(n *Node) {
	if n.Retired == nil {
		n.Retired = map[string]bool{}
	}
	if n.Incarnation != "" {
		n.Retired[n.Incarnation] = true
	}
	n.Incarnation, n.Sequence = "", 0
	n.PairingProgress = nil
	n.Capabilities = []string{}
	n.Snapshot, n.Metrics = map[string]any{}, map[string]any{}
	n.SnapshotAt, n.LastSeen = time.Time{}, time.Time{}
	n.LimitAt = map[string]time.Time{}
}
func (c *Controller) resetNodeLocked(runID string, n *Node, since time.Time) {
	c.stopWatcherLocked(runID, n.ID)
	resetObservation(n)
	n.LogAt = since
	n.LogRows = nil
	n.ProcessMetrics = nil
	n.EffectiveResources = nil
}
func (c *Controller) applyContainerLocked(r *Run, n *Node, containerID, lifecycle string) {
	// A Docker lifecycle request may still be in flight. The action reconciles
	// its target when it returns; polling must not restart its old watcher.
	if n.Lifecycle == "changing" {
		return
	}
	if n.Container != containerID {
		c.resetNodeLocked(r.ID, n, time.Time{})
		n.Retired = map[string]bool{}
		n.AdapterAt = time.Time{}
	}
	if n.Lifecycle != lifecycle {
		c.emitLocked(Event{RunID: r.ID, NodeID: n.ID, Type: "container.state", Data: map[string]any{"state": lifecycle}})
	}
	n.Container, n.Lifecycle = containerID, lifecycle
	key := r.ID + "/" + n.ID
	if lifecycle != "running" && lifecycle != "paused" {
		c.stopWatcherLocked(r.ID, n.ID)
	} else if !c.closed && c.watchers[key] == nil {
		watchCtx, stop := context.WithCancel(c.ctx)
		c.watchers[key] = stop
		c.workers.Add(1)
		go func() { defer c.workers.Done(); c.watch(watchCtx, r.ID, n.ID, containerID) }()
	}
	c.dirty[r.ID] = true
}
func (c *Controller) reconcile(runID string) {
	c.mu.Lock()
	run := c.runs[runID]
	if c.closed || c.docker == nil || run == nil || !reconcilable(run.Status) {
		c.mu.Unlock()
		return
	}
	generation := run.generation
	for _, node := range run.Nodes {
		if node.External != nil {
			c.startPhysicalWatcherLocked(runID, node)
		}
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(c.ctx, 8*time.Second)
	defer cancel()
	containers, err := c.docker.Containers(ctx, runID)
	if err != nil {
		if ctx.Err() == nil {
			c.Emit(Event{RunID: runID, Type: "runtime.error", Data: map[string]any{"error": err.Error()}})
		}
		return
	}
	var workers sync.WaitGroup
	limit := make(chan struct{}, 24)
	present := map[string]bool{}
	for _, container := range containers {
		labels, _ := container["Labels"].(map[string]any)
		nodeID := fmt.Sprint(labels["org.eebus-arena.node"])
		containerID, lifecycle := fmt.Sprint(container["Id"]), fmt.Sprint(container["State"])
		c.mu.Lock()
		r, n, err := c.nodeLocked(runID, nodeID)
		if c.closed || err != nil || r != run || r.generation != generation || !reconcilable(r.Status) {
			c.mu.Unlock()
			continue
		}
		present[nodeID] = true
		c.applyContainerLocked(r, n, containerID, lifecycle)
		c.mu.Unlock()
		if lifecycle == "running" || lifecycle == "paused" || lifecycle == "exited" {
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				workers.Wait()
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-limit }()
				if lifecycle == "exited" {
					c.inspectResourceLimits(ctx, run, generation, nodeID, containerID)
				} else {
					c.sampleNode(ctx, run, generation, nodeID, containerID)
				}
			}()
		}
	}
	c.mu.Lock()
	if !c.closed && c.runs[runID] == run && run.generation == generation && reconcilable(run.Status) {
		for _, n := range run.Nodes {
			if !present[n.ID] && n.Container != "" {
				c.resetNodeLocked(runID, n, time.Time{})
				n.Container, n.Lifecycle = "", "missing"
				c.dirty[runID] = true
				c.emitLocked(Event{RunID: runID, NodeID: n.ID, Type: "container.state", Data: map[string]any{"state": "missing"}})
			}
		}
	}
	c.mu.Unlock()
	workers.Wait()
}

// Lifecycle actions inspect only their target; the regular poll samples all nodes.
func (c *Controller) reconcileNode(ctx context.Context, runID, nodeID, container string) {
	c.mu.Lock()
	run := c.runs[runID]
	if run == nil || c.closed || !reconcilable(run.Status) {
		c.mu.Unlock()
		return
	}
	generation := run.generation
	c.mu.Unlock()
	var info struct {
		State  struct{ Status string }
		Config struct{ Labels map[string]string }
	}
	if err := c.docker.JSON(ctx, "GET", "/containers/"+container+"/json", nil, &info); err != nil {
		return
	}
	if info.Config.Labels["org.eebus-arena.run"] != runID || info.Config.Labels["org.eebus-arena.node"] != nodeID {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, n, err := c.nodeLocked(runID, nodeID)
	if err == nil && !c.closed && r == run && r.generation == generation && reconcilable(r.Status) && n.Container == container {
		c.applyContainerLocked(r, n, container, info.State.Status)
	}
}
func (c *Controller) sampleNode(ctx context.Context, run *Run, generation uint64, nodeID, containerID string) {
	statsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.inspectResourceLimits(statsCtx, run, generation, nodeID, containerID)
	var stat map[string]any
	err := c.docker.JSON(statsCtx, "GET", "/containers/"+containerID+"/stats?stream=false&one-shot=true", nil, &stat)
	c.mu.Lock()
	defer c.mu.Unlock()
	r, n, lookupErr := c.nodeLocked(run.ID, nodeID)
	if c.closed || lookupErr != nil || r != run || r.generation != generation || !reconcilable(r.Status) || n.Container != containerID {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			c.emitLocked(Event{RunID: run.ID, NodeID: nodeID, Type: "metrics.unavailable", Data: map[string]any{"error": err.Error()}})
		}
		return
	}
	metrics := dockerMetrics(stat)
	interval := 0.0
	switch previousAt := n.Metrics["at"].(type) {
	case time.Time:
		interval = metrics["at"].(time.Time).Sub(previousAt).Seconds()
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, previousAt); err == nil {
			interval = metrics["at"].(time.Time).Sub(parsed).Seconds()
		}
	}
	usage := num(nested(stat, "cpu_stats", "cpu_usage", "total_usage"))
	system := num(nested(stat, "cpu_stats", "system_cpu_usage"))
	previousSystem, previousUsage := num(n.Metrics["systemCounter"]), num(n.Metrics["cpuCounter"])
	if previousSystem > 0 && system > previousSystem && usage >= previousUsage {
		metrics["cpuPercent"] = (usage - previousUsage) / (system - previousSystem) * num(nested(stat, "cpu_stats", "online_cpus")) * 100
	}
	metrics["cpuCounter"], metrics["systemCounter"] = usage, system
	n.Metrics = metrics
	if c.resources != nil {
		c.resources.enqueue(run.ID, ResourceRecord{At: metrics["at"].(time.Time), NodeID: n.ID, Container: containerID, Type: "container", Metrics: metrics, IntervalSeconds: interval, Complete: interval > 0 && interval <= 10})
	}
	c.dirty[run.ID] = true
	// State polling and Prometheus expose metrics; avoid flooding the evidence stream.
}
func num(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}
func nested(m map[string]any, keys ...string) any {
	var value any = m
	for _, key := range keys {
		n, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = n[key]
	}
	return value
}
func dockerMetrics(s map[string]any) map[string]any {
	cpuDelta := num(nested(s, "cpu_stats", "cpu_usage", "total_usage")) - num(nested(s, "precpu_stats", "cpu_usage", "total_usage"))
	sysDelta := num(nested(s, "cpu_stats", "system_cpu_usage")) - num(nested(s, "precpu_stats", "system_cpu_usage"))
	var cpu any
	if cpuDelta >= 0 && sysDelta > 0 && num(nested(s, "precpu_stats", "system_cpu_usage")) > 0 {
		cpu = cpuDelta / sysDelta * num(nested(s, "cpu_stats", "online_cpus")) * 100
	}
	memory := num(nested(s, "memory_stats", "usage"))
	working := memory - num(nested(s, "memory_stats", "stats", "inactive_file"))
	if working < 0 {
		working = memory
	}
	rx, tx, read, write := 0.0, 0.0, 0.0, 0.0
	if networks, ok := s["networks"].(map[string]any); ok {
		for _, network := range networks {
			if n, ok := network.(map[string]any); ok {
				rx += num(n["rx_bytes"])
				tx += num(n["tx_bytes"])
			}
		}
	}
	if rows, ok := nested(s, "blkio_stats", "io_service_bytes_recursive").([]any); ok {
		for _, row := range rows {
			if r, ok := row.(map[string]any); ok {
				switch strings.ToLower(fmt.Sprint(r["op"])) {
				case "read":
					read += num(r["value"])
				case "write":
					write += num(r["value"])
				}
			}
		}
	}
	metrics := map[string]any{"memoryLimitBytes": nested(s, "memory_stats", "limit"), "at": time.Now().UTC()}
	if cpu != nil {
		metrics["cpuPercent"] = cpu
	}
	if nested(s, "memory_stats", "usage") != nil {
		metrics["memoryBytes"], metrics["workingSetBytes"] = memory, working
	}
	if s["networks"] != nil {
		metrics["rxBytes"], metrics["txBytes"] = rx, tx
	}
	if nested(s, "blkio_stats", "io_service_bytes_recursive") != nil {
		metrics["blockReadBytes"], metrics["blockWriteBytes"] = read, write
	}
	return metrics
}
func (c *Controller) watch(ctx context.Context, runID, nodeID, container string) {
	c.mu.Lock()
	_, node, lookupErr := c.nodeLocked(runID, nodeID)
	if lookupErr != nil {
		c.mu.Unlock()
		return
	}
	since := node.LogAt
	boundary := map[string]int{}
	for key, count := range node.LogRows {
		boundary[key] = count
	}
	c.mu.Unlock()
	disconnected := false
	delay := 2 * time.Second
	for ctx.Err() == nil {
		seen := map[string]int{}
		err := c.docker.Logs(ctx, container, since, func(stream byte, line string) {
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				return
			}
			stamp, err := time.Parse(time.RFC3339Nano, parts[0])
			if err != nil {
				return
			}
			key := fmt.Sprintf("%d:%x", stream, sha256.Sum256([]byte(parts[1])))
			if stamp.Before(since) {
				// Docker can multiplex an older stdout line after newer stderr.
				// Native sequence/incarnation gates, not the cross-stream log
				// watermark, decide whether protocol telemetry is fresh.
				if stream == 1 {
					var event Event
					if json.Unmarshal([]byte(parts[1]), &event) == nil && event.Type != "" {
						c.mu.Lock()
						event.RunID, event.NodeID = runID, nodeID
						if !c.closed && ctx.Err() == nil {
							c.acceptFromLocked(event, container)
						}
						c.mu.Unlock()
					}
				}
				return
			}
			if stamp.Equal(since) {
				seen[key]++
				if seen[key] <= boundary[key] {
					return
				}
			} else {
				since = stamp
				seen = map[string]int{key: 1}
				boundary = map[string]int{}
			}
			boundary[key] = seen[key]
			c.mu.Lock()
			defer c.mu.Unlock()
			_, n, lookupErr := c.nodeLocked(runID, nodeID)
			if lookupErr != nil || c.closed || n.Container != container || ctx.Err() != nil {
				return
			}
			n.LogAt = stamp
			n.LogRows = map[string]int{}
			for key, count := range boundary {
				n.LogRows[key] = count
			}
			c.dirty[runID] = true
			disconnected, delay = false, 2*time.Second
			if stream == 1 {
				var event Event
				if json.Unmarshal([]byte(parts[1]), &event) == nil && event.Type != "" {
					event.RunID, event.NodeID = runID, nodeID
					c.acceptFromLocked(event, container)
					return
				}
			}
			c.emitLocked(Event{RunID: runID, NodeID: nodeID, At: stamp, Type: "log", Data: map[string]any{"stream": stream, "line": parts[1]}})
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil && !disconnected {
			c.Emit(Event{RunID: runID, NodeID: nodeID, Type: "telemetry.disconnected", Data: map[string]any{"error": err.Error()}})
			disconnected = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}
func (c *Controller) accept(e Event) {
	c.acceptFrom(e, "")
}
func (c *Controller) acceptFrom(e Event, container string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acceptFromLocked(e, container)
}
func (c *Controller) acceptFromLocked(e Event, container string) {
	r, n, err := c.nodeLocked(e.RunID, e.NodeID)
	if err != nil {
		return
	}
	if c.closed || r.Status == "editing" || container != "" && n.Container != container {
		return
	}
	if strings.HasPrefix(e.Type, "resource.") {
		c.acceptResourcesLocked(r, n, e)
		return
	}
	if e.Incarnation == "" || e.Sequence == 0 {
		c.emitLocked(Event{RunID: e.RunID, NodeID: e.NodeID, Type: "telemetry.gap", Data: map[string]any{"reason": "adapter event missing incarnation or sequence"}})
		return
	}
	if n.Incarnation != e.Incarnation {
		if n.Retired[e.Incarnation] || !e.At.IsZero() && e.At.Before(n.AdapterAt) {
			return
		}
		resetObservation(n)
		n.Incarnation = e.Incarnation
		n.Sequence = 0
		n.Capabilities = []string{}
		n.Snapshot = map[string]any{}
		n.SnapshotAt = time.Time{}
	}
	if e.Sequence <= n.Sequence {
		return
	}
	if e.Sequence != n.Sequence+1 {
		c.emitLocked(Event{RunID: e.RunID, NodeID: e.NodeID, Type: "telemetry.gap", Data: map[string]any{"expected": n.Sequence + 1, "received": e.Sequence}})
	}
	n.Sequence = e.Sequence
	n.LastSeen = time.Now().UTC()
	if e.At.After(n.AdapterAt) {
		n.AdapterAt = e.At
	}
	if e.Type == "capabilities" {
		n.Capabilities = []string{}
		if values, ok := e.Data["commands"].([]any); ok {
			for _, v := range values {
				n.Capabilities = append(n.Capabilities, fmt.Sprint(v))
			}
		}
	}
	if e.Type == "snapshot" {
		if e.Data == nil {
			c.emitLocked(Event{RunID: e.RunID, NodeID: e.NodeID, Type: "telemetry.gap", Data: map[string]any{"reason": "adapter snapshot missing data"}})
			c.dirty[r.ID] = true
			return
		}
		unchanged := jsonEqual(n.Snapshot, e.Data)
		for _, uc := range []string{"lpc", "lpp"} {
			if !jsonEqual(nested(n.Snapshot, uc, "value"), nested(e.Data, uc, "value")) || !jsonEqual(nested(n.Snapshot, uc, "active"), nested(e.Data, uc, "active")) {
				if n.LimitAt == nil {
					n.LimitAt = map[string]time.Time{}
				}
				n.LimitAt[uc] = n.LastSeen
			}
		}
		n.Snapshot = e.Data
		n.SnapshotAt = time.Now().UTC()
		if values, ok := e.Data["monitoring"].(map[string]any); ok {
			n.MonitoringObservation = &MonitoringObservation{At: n.SnapshotAt, Incarnation: n.Incarnation, Values: values}
		}
		c.observePairingProgress(r, n)
		if unchanged {
			c.dirty[r.ID] = true
			return
		}
	}
	if e.Type == "usecase.limit" {
		uc := strings.ToLower(fmt.Sprint(e.Data["usecase"]))
		n.Snapshot[uc] = e.Data
		n.SnapshotAt = time.Now().UTC()
		if n.LimitAt == nil {
			n.LimitAt = map[string]time.Time{}
		}
		n.LimitAt[uc] = n.SnapshotAt
	}
	if e.Type == "ship.state" {
		peers, ok := n.Snapshot["peers"].(map[string]any)
		if !ok {
			peers = map[string]any{}
			n.Snapshot["peers"] = peers
		}
		peers[fmt.Sprint(e.Data["ski"])] = e.Data["state"]
		n.SnapshotAt = n.LastSeen
	}
	c.emitLocked(e)
	c.dirty[r.ID] = true
}
func (c *Controller) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, r := range c.runs {
		r.generation++
		if Contains([]string{"starting", "testing", "stopping"}, r.Status) {
			r.Status = "interrupted"
		}
	}
	c.cancel()
	for _, cancel := range c.scenarios {
		cancel()
	}
	c.mu.Unlock()
	c.workers.Wait()
	if c.resources != nil {
		c.resources.close()
	}
	c.mu.Lock()
	for _, r := range c.runs {
		c.saveLocked(r)
	}
	c.mu.Unlock()
	close(c.saveStop)
	c.saveWorkers.Wait()
	c.flushSaves()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, file := range c.journal {
		file.Sync()
		file.Close()
	}
	c.journal = map[string]*os.File{}
}
