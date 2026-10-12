package arena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrRunNotFound = errors.New("arena not found")
var ErrRunActive = errors.New("stop the arena and wait for its scenario to finish before dropping it")

// Drop moves a run's files to dropped/. Restore can undo it, and a controller
// restart does not reload it. It never stops nodes.
func (c *Controller) Drop(runID string) error {
	c.mu.Lock()
	locked := true
	defer func() {
		if locked {
			c.mu.Unlock()
		}
	}()
	r := c.runs[runID]
	if r == nil {
		return ErrRunNotFound
	}
	if c.closed || (r.Status != "created" && r.Status != "stopped" && r.Status != "corrupt" && r.Status != "archived") || c.scenarios[runID] != nil {
		return ErrRunActive
	}
	if _, starting := c.starts[runID]; starting {
		return ErrRunActive
	}
	for _, node := range r.Nodes {
		if node.Container != "" {
			return ErrRunActive
		}
	}
	c.workers.Add(1)
	defer c.workers.Done()
	previous := r.Status
	r.Status = "dropping"
	r.generation++
	c.mu.Unlock()
	locked = false
	c.flushSaves()
	defer func() {
		if !locked {
			c.mu.Lock()
			defer c.mu.Unlock()
		}
		if c.runs[runID] == r && r.Status == "dropping" {
			r.Status = previous
		}
	}()
	if c.resources != nil {
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		err := c.resources.release(ctx, runID)
		cancel()
		if err != nil {
			return err
		}
	}
	if c.docker != nil {
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		defer cancel()
		// Ask the daemon too: the saved state can miss containers started outside.
		for _, find := range []func(context.Context, string) ([]map[string]any, error){c.docker.Containers, c.docker.ProjectContainers} {
			containers, err := find(ctx, runID)
			if err != nil {
				return fmt.Errorf("checking arena containers: %w", err)
			}
			if len(containers) != 0 {
				return ErrRunActive
			}
		}
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	locked = true
	if c.closed || c.runs[runID] != r || r.Status != "dropping" {
		return ErrRunActive
	}
	if c.dirty[runID] {
		return errors.New("arena state could not be saved; repair storage before dropping it")
	}
	r.Status = previous
	archive := filepath.Join(c.data, "dropped", runID)
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		return fmt.Errorf("preserving dropped arena: %w", err)
	}
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		return errors.New("a dropped copy of this arena already exists or cannot be read")
	}
	if err := os.Rename(c.path(runID, ""), archive); err != nil {
		return fmt.Errorf("dropping arena: %w", err)
	}
	if journal := c.journal[runID]; journal != nil {
		journal.Close()
		delete(c.journal, runID)
	}
	for _, node := range r.Nodes {
		key := runID + "/" + node.ID
		if cancel := c.watchers[key]; cancel != nil {
			cancel()
			delete(c.watchers, key)
		}
	}
	delete(c.pendingSaves, runID)
	delete(c.dirty, runID)
	delete(c.runs, runID)
	if c.resources != nil {
		c.resources.forget(runID)
	}
	// Remove the run from the replay history. The archive keeps its evidence.
	retained := c.events[:0]
	for _, event := range c.events {
		if event.RunID != runID {
			retained = append(retained, event)
		}
	}
	c.events = retained
	c.emitLocked(Event{RunID: runID, Type: "run.dropped"})
	return nil
}

func (c *Controller) Restore(runID string) error {
	if !safeID.MatchString(runID) {
		return ErrRunNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("controller is closed")
	}
	if c.runs[runID] != nil {
		return errors.New("arena is already restored")
	}
	archive := filepath.Join(c.data, "dropped", runID)
	data, err := os.ReadFile(filepath.Join(archive, "run.json"))
	if errors.Is(err, os.ErrNotExist) {
		if info, statErr := os.Stat(archive); statErr != nil || !info.IsDir() {
			return ErrRunNotFound
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var run Run
	err = json.Unmarshal(data, &run)
	if err != nil || run.ID != runID {
		run = Run{ID: runID, Definition: Definition{Name: "Unreadable arena"}, Status: "corrupt", Error: "restored unreadable arena; original files preserved", Nodes: []*Node{}, Assertions: []Assertion{}}
	}
	if run.Status != "created" && run.Status != "stopped" && run.Status != "corrupt" && run.Status != "archived" {
		return errors.New("invalid dropped arena")
	}
	for _, node := range run.Nodes {
		if node.Container != "" {
			return errors.New("dropped arena still references a container")
		}
	}
	if _, err := os.Stat(c.path(runID, "")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("arena directory already exists or cannot be read")
	}
	if err := os.Rename(archive, c.path(runID, "")); err != nil {
		return fmt.Errorf("restoring arena: %w", err)
	}
	if err := c.openJournal(runID); err != nil {
		if rollback := os.Rename(c.path(runID, ""), archive); rollback != nil {
			return fmt.Errorf("opening restored evidence: %v; restoring archive: %w", err, rollback)
		}
		return err
	}
	c.runs[runID] = &run
	if c.resources != nil {
		var status ResourceStatus
		if blob, err := os.ReadFile(c.path(runID, "resource-meta.json")); err == nil && json.Unmarshal(blob, &status) == nil {
			c.resources.mu.Lock()
			c.resources.status[runID] = status
			c.resources.mu.Unlock()
		}
	}
	c.emitLocked(Event{RunID: runID, Type: "run.restored"})
	return nil
}
