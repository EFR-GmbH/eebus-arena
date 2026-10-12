package labagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAgentLeaseAndAuthentication(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native")
	if err := os.WriteFile(native, []byte("printf '%s\\n' '{\"type\":\"capabilities\",\"nodeId\":\"node-001\",\"incarnation\":\"native\",\"sequence\":1}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Root: root, Native: native, Supervisor: "/bin/sh", Token: "test-token"}
	handler := agent.Handler()
	request := func(method, path string, body any, token string) *httptest.ResponseRecorder {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if response := request("GET", "/events?after=0", nil, "wrong-token"); response.Code != 401 {
		t.Fatal("unauthorized agent access")
	}
	session := Session{Run: "run-test", Node: "node-001", Config: map[string]any{"nodeId": "node-001", "role": "EG", "timeScale": 1, "interfaces": []string{"lo"}}, Certificate: "test-cert", PrivateKey: "test-key"}
	if response := request("POST", "/session", session, "test-token"); response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	agent.mu.Lock()
	done := agent.done
	agent.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("native process did not exit")
	}
	if response := request("GET", "/events?run=run-foreign&node=node-001&after=0", nil, "test-token"); response.Code != 409 {
		t.Fatal("foreign lease readable")
	}
	response := request("GET", "/events?run=run-test&node=node-001&after=0", nil, "test-token")
	var batch Batch
	if err := json.Unmarshal(response.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) < 2 || batch.Running {
		t.Fatalf("native events or exit missing: %+v", batch)
	}
	if response = request("POST", "/action?run=run-foreign&node=node-001", map[string]string{"action": "release"}, "test-token"); response.Code != 409 {
		t.Fatal("foreign lease released")
	}
	if response = request("POST", "/action?run=run-test&node=node-001", map[string]string{"action": "release"}, "test-token"); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	key, err := os.Stat(filepath.Join(root, "run-test", "node-001", "key.pem"))
	if err != nil || key.Mode().Perm() != 0600 {
		t.Fatal("identity permissions")
	}
}

func TestPhysicalKillTerminatesDescendantsAndAllowsRestart(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native.sh")
	pidfile := filepath.Join(root, "child.pid")
	t.Setenv("ARENA_TEST_CHILD_PID", pidfile)
	if err := os.WriteFile(native, []byte("sleep 300 &\necho $! > \"$ARENA_TEST_CHILD_PID\"\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{Root: root, Native: native, Supervisor: "/bin/sh", Token: "test-token"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = agent.Shutdown(ctx)
	})
	session := Session{Run: "run-test", Node: "node-001", Config: map[string]any{"nodeId": "node-001", "role": "EG", "timeScale": float64(1), "interfaces": []any{"lo"}}, Certificate: "cert", PrivateKey: "key"}
	if err := agent.start(session); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		blob, _ := os.ReadFile(pidfile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(blob)))
		if pid > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	action := func(action string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		r := httptest.NewRequest("POST", "/action?run=run-test&node=node-001", strings.NewReader(`{"action":"`+action+`"}`)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		agent.Handler().ServeHTTP(w, r)
		return w
	}
	if response := action("kill"); response.Code != 200 {
		t.Fatal("hard kill did not complete", response.Body.String())
	}
	// Group signals are asynchronous: waiting for the supervisor does not wait
	// for its descendants to finish exiting. Zombies and dead tasks cannot run.
	deadline = time.Now().Add(time.Second)
	for {
		blob, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			break
		}
		if err != nil {
			t.Fatal("cannot check native descendant", err)
		}
		_, suffix, _ := strings.Cut(string(blob), ") ")
		if strings.HasPrefix(suffix, "Z ") || strings.HasPrefix(suffix, "X ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native descendant survived kill", string(blob))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if response := action("start"); response.Code != 200 {
		t.Fatal("killed job cannot restart", response.Body.String())
	}
	if response := action("release"); response.Code != 200 {
		t.Fatal("restarted job cannot release", response.Body.String())
	}
}

func TestAgentRejectsPhysicalAcceleration(t *testing.T) {
	agent := &Agent{Root: t.TempDir()}
	session := Session{Run: "run-test", Node: "node-001", Config: map[string]any{"nodeId": "node-001", "role": "EG", "timeScale": float64(10)}}
	if agent.start(session) == nil {
		t.Fatal("uninstrumented physical speedup accepted")
	}
}

func TestSessionRetryCannotReplaceIdentity(t *testing.T) {
	session := Session{Run: "run-test", Node: "node-001", Config: map[string]any{"nodeId": "node-001", "role": "EG", "timeScale": float64(1)}, Certificate: "cert", PrivateKey: "key"}
	agent := &Agent{Root: t.TempDir(), Token: "test-token", session: &session, command: &exec.Cmd{}}
	request := func(s Session) int {
		body, _ := json.Marshal(s)
		r := httptest.NewRequest("POST", "/session", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		agent.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if request(session) != 200 {
		t.Fatal("lost response retry cannot reattach")
	}
	changed := session
	changed.PrivateKey = "different-key"
	if request(changed) != 409 {
		t.Fatal("live identity changed through retry")
	}
	changed = session
	changed.Run = "run-foreign"
	if request(changed) != 409 {
		t.Fatal("foreign lease replaced")
	}
}
