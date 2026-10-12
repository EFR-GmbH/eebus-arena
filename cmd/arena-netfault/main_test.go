package main

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecoveryRespectsReservedRulesAndLease(t *testing.T) {
	for _, test := range []struct {
		name, links, qdisc string
		ingress, fail      bool
		want               int
	}{
		{"egress reserved root", `[]`, `[{"kind":"prio","handle":"a100:"}]`, false, false, 1},
		{"foreign root", `[]`, `[{"kind":"fq_codel","handle":"a100:"}]`, false, true, 0},
		{"already clean", `[]`, `[{"kind":"noqueue","handle":"0:"}]`, false, false, 0},
		{"foreign IFB", `[{"ifname":"arena-ifb","ifalias":"other"}]`, `[]`, true, true, 0},
		{"owned ingress", `[{"ifname":"arena-ifb","ifalias":"fault-aaaaaaaaaaaaaaaaaaaaaaaa"}]`, `[{"kind":"prio","handle":"a100:"},{"kind":"ingress","handle":"ffff:"}]`, true, false, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, output := range map[string]string{"ip": test.links, "tc": test.qdisc} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			var calls [][]string
			execute := func(args ...string) error { calls = append(calls, args); return nil }
			err := restoreRules("fault-aaaaaaaaaaaaaaaaaaaaaaaa", test.ingress, execute, execute)
			if (err != nil) != test.fail || len(calls) != test.want {
				t.Fatalf("err=%v calls=%v", err, calls)
			}
			if test.name == "owned ingress" && !reflect.DeepEqual(calls[2], []string{"link", "del", "arena-ifb"}) {
				t.Fatal(calls)
			}
		})
	}
}

func TestIPv6FlowerFilters(t *testing.T) {
	args := peerFilter(net.ParseIP("fd12::9"), "a100:", "dst", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "protocol ipv6") || !strings.Contains(joined, "dst_ip fd12::9/128") || strings.Contains(joined, "u32") {
		t.Fatal(args)
	}
	args = peerFilter(net.ParseIP("fd12::9"), "ffff:", "src", true)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "src_ip fd12::9/128") || !strings.Contains(joined, "dst_port 5353") || !strings.Contains(joined, "ip_proto udp") {
		t.Fatal(args)
	}
}
