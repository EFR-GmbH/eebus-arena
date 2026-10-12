package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestPairingRateLimitDoesNotSuppressNativeTransport(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "trace")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	previous := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = previous }()
	a := adapter{traceWindow: time.Now(), traceCount: 100}
	a.trace("pairing.discovery", map[string]any{})
	a.trace("ship.transport", map[string]any{"family": "ipv6"})
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	var first, second struct{ Type string }
	if err := decoder.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.Type != "pairing.suppressed" || second.Type != "ship.transport" || a.traceSuppressed != 1 {
		t.Fatal("transport lost in pairing limiter", first.Type, second.Type, a.traceSuppressed)
	}
}
