package main

import (
	ship "github.com/enbility/ship-go/api"
	"os"
	"path/filepath"
	"testing"
)

func TestPairingStoreRestoresTrustAndReplay(t *testing.T) {
	dir := t.TempDir()
	p, err := openPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	i := ship.NewServiceIdentity("0123456789012345678901234567890123456789", "", "peer")
	i.PairingType = ship.PairingTypeAddCu
	if err = p.update(i, false); err != nil {
		t.Fatal(err)
	}
	if err = p.SaveRingBuffer([]ship.DigestEntry{{Algorithm: "hmacSha256", Digest: "saved"}}, 0); err != nil {
		t.Fatal(err)
	}
	p, err = openPairingStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, next, err := p.LoadRingBuffer()
	if err != nil || len(entries) != 1 || entries[0].Digest != "saved" || next != 0 ||
		len(p.identities()) != 1 || p.identities()[0].PairingType != ship.PairingTypeAddCu {
		t.Fatal("persistence lost")
	}
	if err = p.update(i, true); err != nil {
		t.Fatal(err)
	}
	if len(p.identities()) != 0 {
		t.Fatal("removed trust restored")
	}
}

func TestPairingStoreRejectsCorruptState(t *testing.T) {
	for _, data := range []string{
		`no`,
		`{}`,
		`{"version":1}`,
		`{"trusted":{}}`,
		`{"version":1,"trusted":{},"entries":[],"next":1}`,
		`{"version":2,"trusted":{}}`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "pairing.json"), []byte(data), 0600)
		if _, err := openPairingStore(dir); err == nil {
			t.Fatal("corrupt state accepted", data)
		}
	}
}
