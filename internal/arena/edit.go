package arena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrEditActive = errors.New("stop the arena and wait for cleanup to finish before editing it")
var ErrEditConflict = errors.New("the arena setup changed; cancel and reopen Edit arena")

// EditRequest applies a complete definition to the revision shown by the editor.
type EditRequest struct {
	Definition Definition `json:"definition"`
	Revision   uint64     `json:"revision"`
}

type editTransaction struct {
	Archive string `json:"archive"`
	Staging string `json:"staging"`
}

var setupParts = []string{"identities", "state", "bundle.json", "compose.json", "run.json"}

func editableRun(r *Run) bool {
	if r == nil || !Contains([]string{"created", "stopped"}, r.Status) || r.Error != "" {
		return false
	}
	for _, n := range r.Nodes {
		if n.Container != "" || n.External != nil && n.Lifecycle == "unavailable" {
			return false
		}
	}
	return true
}

// Update changes a quiescent arena in place. Provisioning is reused by node ID;
// previous configurations and results remain in revisions/ and the event journal.
func (c *Controller) Update(ctx context.Context, runID string, input EditRequest) (err error) {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil {
		c.mu.Unlock()
		return ErrRunNotFound
	}
	_, starting := c.starts[runID]
	if c.closed || !editableRun(r) || starting || c.scenarios[runID] != nil || c.dirty[runID] {
		c.mu.Unlock()
		return ErrEditActive
	}
	if r.SetupRevision != input.Revision {
		c.mu.Unlock()
		return ErrEditConflict
	}
	variants := make(map[string]Variant, len(c.variants))
	for k, v := range c.variants {
		variants[k] = v
	}
	previousStatus := r.Status
	r.Status = "editing"
	r.generation++
	generation, tester := r.generation, c.tester
	c.workers.Add(1)
	c.mu.Unlock()
	defer c.workers.Done()
	defer func() {
		c.mu.Lock()
		if c.runs[runID] == r && r.Status == "editing" {
			r.Status = previousStatus
		}
		c.mu.Unlock()
	}()
	c.flushSaves()
	c.mu.Lock()
	dirty := c.dirty[runID]
	c.mu.Unlock()
	if dirty {
		return errors.New("arena state could not be saved; repair storage before editing it")
	}
	if c.docker != nil {
		// Saved state may miss containers created outside the controller.
		for _, find := range []func(context.Context, string) ([]map[string]any, error){c.docker.Containers, c.docker.ProjectContainers} {
			containers, lookupErr := find(ctx, runID)
			if lookupErr != nil {
				return fmt.Errorf("checking arena containers: %w", lookupErr)
			}
			if len(containers) != 0 {
				return ErrEditActive
			}
		}
	}
	var old Bundle
	body, err := os.ReadFile(c.path(runID, "bundle.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(body, &old); err != nil {
		return err
	}
	definition := input.Definition
	for i := range definition.Groups {
		next := definition.Groups[i].External
		if next == nil || next.Token != "" {
			continue
		}
		for _, group := range old.Definition.Groups {
			// State polling redacts the token. Retain it only for the same
			// endpoint and certificate, never forward it to a new device.
			if group.External != nil && group.External.Endpoint == next.Endpoint && group.External.TLSFingerprint == next.TLSFingerprint {
				next.Token = group.External.Token
			}
		}
	}
	if err = definition.Validate(variants); err != nil {
		return err
	}
	bundle := Bundle{SchemaVersion: SchemaVersion, Definition: definition, Identities: map[string]Identity{}, Pairing: map[string]PairingCredential{}}
	for _, group := range definition.Groups {
		for slot := 1; slot <= group.Count; slot++ {
			nodeID := fmt.Sprintf("%s-%03d", group.ID, slot)
			if group.Role == "STRESSER" {
				continue
			}
			if identity, ok := old.Identities[nodeID]; ok {
				bundle.Identities[nodeID] = identity
			}
			pairing := false
			for _, link := range definition.Links {
				pairing = pairing || link.Mode == "ship-pairing" && (link.From == group.ID || link.To == group.ID)
			}
			for _, node := range r.Nodes {
				if node.ID == nodeID && node.Role == group.Role && pairing && (node.Stack == "openeebus") == (variants[group.Variant].Stack == "openeebus") {
					if cred, ok := old.Pairing[nodeID]; ok {
						bundle.Pairing[nodeID] = cred
					}
				}
			}
		}
	}
	nodes, err := Resolve(&bundle, variants)
	if err != nil {
		return err
	}
	directory := c.path(runID, "")
	staging, err := os.MkdirTemp(directory, ".edit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = writeNodeSetup(staging, bundle, nodes); err != nil {
		return err
	}
	for _, node := range nodes {
		for _, previous := range r.Nodes {
			if previous.ID != node.ID {
				continue
			}
			// Reject late telemetry from the previous incarnation after saving.
			node.Retired = make(map[string]bool, len(previous.Retired)+1)
			for k, v := range previous.Retired {
				node.Retired[k] = v
			}
			if previous.Incarnation != "" {
				node.Retired[previous.Incarnation] = true
			}
			if previous.Variant == node.Variant && previous.Role == node.Role && sameDevice(previous.External, node.External) {
				source := c.path(runID, filepath.Join("state", node.ID))
				if _, err = os.Stat(source); err == nil {
					if err = os.CopyFS(filepath.Join(staging, "state", node.ID), os.DirFS(source)); err != nil {
						return fmt.Errorf("preserving node state: %w", err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	next := &Run{ID: r.ID, Created: r.Created, SetupRevision: r.SetupRevision + 1, Definition: bundle.Definition, Variants: bundle.Variants, Status: "created", Nodes: nodes, Assertions: []Assertion{}, Runtime: map[string]any{"tester": tester}, MonitoringSequence: r.MonitoringSequence, generation: generation}
	for name, value := range map[string]any{"bundle.json": bundle, "compose.json": c.compose(next, variants), "run.json": next} {
		if err = writeJSON(filepath.Join(staging, name), value); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	transaction := editTransaction{Archive: "edit-" + id(), Staging: filepath.Base(staging)}
	archive := filepath.Join(directory, "revisions", transaction.Archive)
	if err = os.MkdirAll(archive, 0700); err != nil {
		return err
	}
	// Exclude persistence and exports while publishing the five setup parts.
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.runs[runID] != r || r.Status != "editing" {
		return ErrEditActive
	}
	if err = writeJSON(filepath.Join(directory, ".edit.json"), transaction); err != nil {
		return err
	}
	for _, name := range setupParts {
		if err = os.Rename(filepath.Join(directory, name), filepath.Join(archive, name)); err == nil {
			err = os.Rename(filepath.Join(staging, name), filepath.Join(directory, name))
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = os.Remove(filepath.Join(directory, ".edit.json"))
	}
	if err != nil {
		if rollbackErr := recoverEdit(directory); rollbackErr != nil {
			r.Status, r.Error = "corrupt", "edit recovery failed: "+rollbackErr.Error()
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	c.runs[runID] = next
	c.emitLocked(Event{RunID: runID, Type: "run.edited", Data: map[string]any{"revision": next.SetupRevision, "previousSetup": filepath.Join("revisions", transaction.Archive), "nodes": len(nodes)}})
	c.saveLocked(next)
	return nil
}

func sameDevice(a, b *ExternalNode) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Endpoint == b.Endpoint && a.TLSFingerprint == b.TLSFingerprint
}

// recoverEdit rolls back an interrupted publication before any run is loaded.
func recoverEdit(directory string) error {
	marker := filepath.Join(directory, ".edit.json")
	body, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var transaction editTransaction
	if err = json.Unmarshal(body, &transaction); err != nil {
		return err
	}
	if !safeID.MatchString(transaction.Archive) || !strings.HasPrefix(transaction.Archive, "edit-") || !strings.HasPrefix(transaction.Staging, ".edit-") || filepath.Base(transaction.Staging) != transaction.Staging {
		return errors.New("invalid edit recovery marker")
	}
	archive := filepath.Join(directory, "revisions", transaction.Archive)
	for _, name := range setupParts {
		backup := filepath.Join(archive, name)
		if _, err = os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err = os.RemoveAll(filepath.Join(directory, name)); err != nil {
			return err
		}
		if err = os.Rename(backup, filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	if err = os.RemoveAll(filepath.Join(directory, transaction.Staging)); err != nil {
		return err
	}
	if err = os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(marker)
}
