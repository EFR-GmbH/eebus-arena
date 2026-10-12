// arena-netfault applies a bounded impairment inside one owned container namespace.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	addresses := flag.Bool("addresses", false, "Read eth0 addresses without modifying the namespace")
	peer := flag.String("peer", "", "Peer IPv4 or IPv6 address")
	ingress := flag.Bool("ingress", false, "Impair traffic received from this peer")
	mdns := flag.Bool("mdns", false, "Select only UDP mDNS traffic")
	delay := flag.Int("delay", 0, "Delay in real milliseconds")
	jitter := flag.Int("jitter", 0, "Jitter in real milliseconds")
	loss := flag.Float64("loss", 0, "Loss percentage")
	rate := flag.Int("rate", 0, "Bandwidth in kbit/s; zero is unlimited")
	duration := flag.Int("duration", 0, "Maximum lifetime in real seconds")
	lease := flag.String("lease", "", "Arena fault lease identity")
	restoreOnly := flag.Bool("restore-only", false, "Restore a terminated helper's owned rules")
	flag.Parse()
	if *addresses {
		iface, err := net.InterfaceByName("eth0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		values, err := iface.Addrs()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		var addresses []string
		for _, value := range values {
			addresses = append(addresses, value.String())
		}
		if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"addresses": addresses}); err != nil {
			return 1
		}
		return 0
	}
	ip := net.ParseIP(*peer)
	if !regexp.MustCompile(`^fault-[a-f0-9]{24}$`).MatchString(*lease) || !*restoreOnly && (ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || *delay < 0 || *delay > 10000 || *jitter < 0 || *jitter > *delay || math.IsNaN(*loss) || math.IsInf(*loss, 0) || *loss < 0 || *loss > 100 || *rate < 0 || *rate > 1000000 || *duration < 1 || *duration > 600 || *mdns && !*ingress) {
		fmt.Fprintln(os.Stderr, "invalid or unscoped impairment")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	tc := func(args ...string) error {
		commandCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(commandCtx, "tc", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("tc %v: %w: %s", args, err, out)
		}
		return nil
	}
	ipCommand := func(args ...string) error {
		commandCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(commandCtx, "ip", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ip %v: %w: %s", args, err, out)
		}
		return nil
	}
	device, parent, direction := "eth0", "a100:", "dst"
	if *restoreOnly {
		if err := restoreRules(*lease, *ingress, tc, ipCommand); err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.cleanup_failed", "error": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.restored", "at": time.Now().UTC(), "recovered": true})
		return 0
	}
	rootCreated, ifbCreated, ingressCreated := false, false, false
	cleanup := func() error {
		var failure error
		if ingressCreated {
			if err := tc("qdisc", "del", "dev", "eth0", "ingress"); err != nil {
				failure = err
			}
		}
		if rootCreated {
			args := []string{"qdisc", "del", "dev", device, "root"}
			if err := tc(args...); err != nil {
				failure = err
			}
		}
		if ifbCreated {
			if err := ipCommand("link", "del", "arena-ifb"); err != nil {
				failure = err
			}
		}
		return failure
	}
	defer func() {
		if err := cleanup(); err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.cleanup_failed", "error": err.Error()})
		}
	}()
	if *ingress {
		if err := ipCommand("link", "add", "arena-ifb", "type", "ifb"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ifbCreated = true
		if err := ipCommand("link", "set", "arena-ifb", "alias", *lease); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := ipCommand("link", "set", "arena-ifb", "up"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := tc("qdisc", "add", "dev", "eth0", "handle", "ffff:", "ingress"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		ingressCreated = true
		device = "arena-ifb"
		direction = "src"
		parent = "ffff:"
	}
	if err := tc("qdisc", "add", "dev", device, "root", "handle", "a100:", "prio", "bands", "3"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rootCreated = true
	args := []string{"qdisc", "add", "dev", device, "parent", "a100:3", "handle", "a103:", "netem"}
	if *delay > 0 {
		args = append(args, "delay", strconv.Itoa(*delay)+"ms")
		if *jitter > 0 {
			args = append(args, strconv.Itoa(*jitter)+"ms")
		}
	}
	if *loss > 0 {
		args = append(args, "loss", strconv.FormatFloat(*loss, 'f', 3, 64)+"%")
	}
	if *rate > 0 {
		args = append(args, "rate", strconv.Itoa(*rate)+"kbit")
	}
	if len(args) == 9 {
		args = append(args, "delay", "0ms")
	}
	if err := tc(args...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	filter := peerFilter(ip, parent, direction, *mdns)
	if *ingress {
		filter = append(filter, "action", "mirred", "egress", "redirect", "dev", device)
	} else {
		filter = append(filter, "flowid", "a100:3")
	}
	if err := tc(filter...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *ingress {
		// Only matched peer packets reach this IFB; steer those into the netem band.
		if err := tc("filter", "add", "dev", device, "protocol", "all", "parent", "a100:", "prio", "1", "matchall", "flowid", "a100:3"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.applied", "peer": ip.String(), "ingress": *ingress, "mdns": *mdns, "delayMillis": *delay, "jitterMillis": *jitter, "lossPercent": *loss, "rateKbit": *rate, "at": time.Now().UTC()})
	statistics := func() {
		statsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(statsCtx, "tc", "-s", "-j", "qdisc", "show", "dev", device).Output()
		if err != nil || len(out) > 1<<20 || !json.Valid(out) {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.statistics", "at": time.Now().UTC(), "error": fmt.Sprint(err), "available": false})
			return
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.statistics", "at": time.Now().UTC(), "device": device, "qdisc": json.RawMessage(out), "available": true})
	}
	statistics()
	timer := time.NewTimer(time.Duration(*duration) * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-timer.C:
			break wait
		case <-ticker.C:
			statistics()
		}
	}
	statistics()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rootCreated = false
	ingressCreated = false
	ifbCreated = false
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "fault.restored", "at": time.Now().UTC()})
	return 0
}

func peerFilter(ip net.IP, parent, direction string, mdns bool) []string {
	protocol, prefix := "ip", "/32"
	if ip.To4() == nil {
		protocol, prefix = "ipv6", "/128"
	}
	args := []string{"filter", "add", "dev", "eth0", "protocol", protocol, "parent", parent, "prio", "1", "flower", direction + "_ip", ip.String() + prefix}
	if mdns {
		args = append(args, "ip_proto", "udp", "dst_port", "5353")
	}
	return args
}

// These names are reserved inside the controller-verified node namespace. The
// IFB alias binds ingress rules to a specific lease; foreign links are untouched.
func restoreRules(lease string, ingress bool, tc, ipCommand func(...string) error) error {
	read := func(program string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, program, args...).Output()
		if err != nil || len(out) > 1<<20 {
			return nil, fmt.Errorf("inspect %s: %w", program, err)
		}
		return out, nil
	}
	links, err := read("ip", "-j", "link", "show")
	if err != nil {
		return err
	}
	var devices []struct {
		Name  string `json:"ifname"`
		Alias string `json:"ifalias"`
	}
	if err = json.Unmarshal(links, &devices); err != nil {
		return err
	}
	ifb := false
	for _, device := range devices {
		if device.Name == "arena-ifb" {
			if device.Alias != lease {
				return fmt.Errorf("IFB lease ownership mismatch")
			}
			ifb = true
		}
	}
	removeRoot := func(device string) error {
		out, err := read("tc", "-j", "qdisc", "show", "dev", device)
		if err != nil {
			return err
		}
		var qdiscs []struct {
			Kind   string `json:"kind"`
			Handle string `json:"handle"`
		}
		if err = json.Unmarshal(out, &qdiscs); err != nil {
			return err
		}
		for _, q := range qdiscs {
			if q.Handle == "a100:" {
				if q.Kind != "prio" {
					return fmt.Errorf("reserved root has unexpected kind %s", q.Kind)
				}
				return tc("qdisc", "del", "dev", device, "root", "handle", "a100:")
			}
		}
		return nil
	}
	if !ingress {
		return removeRoot("eth0")
	}
	if !ifb {
		return nil
	}
	if err = removeRoot("arena-ifb"); err != nil {
		return err
	}
	out, err := read("tc", "-j", "qdisc", "show", "dev", "eth0")
	if err != nil {
		return err
	}
	var qdiscs []struct {
		Kind   string `json:"kind"`
		Handle string `json:"handle"`
	}
	if err = json.Unmarshal(out, &qdiscs); err != nil {
		return err
	}
	for _, q := range qdiscs {
		if q.Handle == "ffff:" {
			if q.Kind != "ingress" {
				return fmt.Errorf("unexpected ingress kind")
			}
			if err = tc("qdisc", "del", "dev", "eth0", "ingress"); err != nil {
				return err
			}
		}
	}
	return ipCommand("link", "del", "arena-ifb")
}
