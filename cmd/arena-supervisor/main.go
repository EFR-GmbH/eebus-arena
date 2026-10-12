// arena-supervisor forwards native adapter traffic and emits independent resource samples.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/resourceprobe"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: arena-supervisor COMMAND [ARGS...]")
		return 2
	}
	var cfg struct {
		NodeID   string `json:"nodeId"`
		Interval int    `json:"resourceIntervalMillis"`
		Scope    string `json:"resourceScope,omitempty"`
	}
	configPath := os.Getenv("ARENA_CONFIG")
	if configPath == "" {
		configPath = "/identity/config.json"
	}
	blob, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = json.Unmarshal(blob, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if cfg.Interval == 0 {
		cfg.Interval = 1000
	}
	if cfg.Interval < 250 || cfg.Interval > 5000 || cfg.NodeID == "" {
		fmt.Fprintln(os.Stderr, "invalid resource collection configuration")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	command := exec.CommandContext(ctx, os.Args[1], os.Args[2:]...)
	command.Stdin = os.Stdin
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cfg.Scope == "device" {
		// The lab agent owns the supervisor's process group. Keep every native
		// descendant in that group so a hard kill cannot leave a live stub.
		command.SysProcAttr = nil
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		if cfg.Scope == "device" {
			return command.Process.Signal(syscall.SIGTERM)
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	}
	command.WaitDelay = 5 * time.Second
	var writeMu sync.Mutex
	write := func(blob []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := os.Stdout.Write(append(blob, '\n')); err != nil {
			stop()
		}
	}
	framer := &lineWriter{publish: write, onError: stop}
	// Let os/exec own the copy goroutine: Wait then joins all stdout reads.
	command.Stdout = framer
	if err = command.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	probe, probeErr := resourceprobe.New()
	if probeErr == nil && cfg.Scope == "device" {
		probe.CgroupRoot = ""
		probe.Device = true
	}
	identity := make([]byte, 12)
	if _, err := rand.Read(identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		stop()
		_ = command.Wait()
		return 1
	}
	incarnation := "collector-" + hex.EncodeToString(identity)
	collectorCtx, cancelCollector := context.WithCancel(ctx)
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Millisecond)
		defer ticker.Stop()
		sequence := uint64(0)
		for {
			sequence++
			var sample resourceprobe.Sample
			err := probeErr
			if err == nil {
				sample, err = probe.Collect()
				sample.Scope = cfg.Scope
			}
			event := map[string]any{"nodeId": cfg.NodeID, "incarnation": incarnation, "sequence": sequence, "at": time.Now().UTC(), "type": "resource.process", "data": sample}
			if err != nil {
				event["type"] = "resource.unavailable"
				event["data"] = map[string]any{"error": err.Error()}
			}
			encoded, err := json.Marshal(event)
			if err == nil {
				write(encoded)
			} else {
				fmt.Fprintln(os.Stderr, "resource encoding:", err)
			}
			select {
			case <-collectorCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	err = command.Wait()
	framer.flush()
	cancelCollector()
	<-collectorDone
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type lineWriter struct {
	pending []byte
	publish func([]byte)
	onError func()
}

func (w *lineWriter) Write(data []byte) (int, error) {
	consumed := 0
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		part := data
		if end >= 0 {
			part = data[:end]
		}
		if len(w.pending)+len(part) > 1<<20 {
			if w.onError != nil {
				w.onError()
			}
			return consumed, fmt.Errorf("native stdout frame exceeds 1 MiB")
		}
		w.pending = append(w.pending, part...)
		consumed += len(part)
		if end < 0 {
			break
		}
		w.publish(w.pending)
		w.pending = nil
		consumed++
		data = data[end+1:]
	}
	return consumed, nil
}

func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		w.publish(w.pending)
		w.pending = nil
	}
}
