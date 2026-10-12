package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFrameLimitAppliesToEachLine(t *testing.T) {
	var frames [][]byte
	w := &lineWriter{publish: func(p []byte) { frames = append(frames, append([]byte(nil), p...)) }}
	large := bytes.Repeat([]byte("a"), (1<<20)-1)
	if _, err := w.Write(large); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("a\nnext\nlast")); err != nil {
		t.Fatal(err)
	}
	w.flush()
	if len(frames) != 3 || len(frames[0]) != 1<<20 || string(frames[1]) != "next" || string(frames[2]) != "last" {
		t.Fatal("split or final frame lost")
	}
}

func TestDeviceSupervisorSharesManagedProcessGroup(t *testing.T) {
	if os.Getenv("ARENA_SUPERVISOR_TEST") == "1" {
		os.Args = []string{"arena-supervisor", "/bin/sh", os.Getenv("ARENA_TEST_NATIVE")}
		os.Exit(run())
	}
	root := t.TempDir()
	config, native, pidfile := filepath.Join(root, "config.json"), filepath.Join(root, "native.sh"), filepath.Join(root, "native.pid")
	blob, _ := json.Marshal(map[string]any{"nodeId": "node-001", "resourceScope": "device"})
	if err := os.WriteFile(config, blob, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("echo $$ > \"$ARENA_TEST_NATIVE_PID\"\nsleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestDeviceSupervisorSharesManagedProcessGroup$")
	command.Env = append(os.Environ(), "ARENA_SUPERVISOR_TEST=1", "ARENA_CONFIG="+config, "ARENA_TEST_NATIVE="+native, "ARENA_TEST_NATIVE_PID="+pidfile)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = time.Second
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL); _ = command.Wait() })
	pid := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("native stub did not start")
	}
	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if group != command.Process.Pid {
		_ = syscall.Kill(-group, syscall.SIGKILL)
		t.Fatal("native escaped managed supervisor group", group, command.Process.Pid)
	}
	if err = syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("hard-killed supervisor exited successfully")
	}
}

func TestOversizedNativeFrameStopsPublisher(t *testing.T) {
	stopped := false
	w := &lineWriter{publish: func([]byte) { t.Fatal("oversized frame published") }, onError: func() { stopped = true }}
	if _, err := w.Write(bytes.Repeat([]byte("a"), (1<<20)+1)); err == nil || !stopped {
		t.Fatal("unbounded native frame accepted")
	}
}
