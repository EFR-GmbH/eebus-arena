package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/EFR-GmbH/eebus-arena/internal/arena"
	"github.com/EFR-GmbH/eebus-arena/internal/mdnsstress"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	data, err := os.ReadFile("/identity/config.json")
	if err != nil {
		return err
	}
	var cfg struct {
		NodeID      string               `json:"nodeId"`
		NetworkMode string               `json:"networkMode"`
		Stress      *arena.StressProfile `json:"stress"`
	}
	if err = json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if cfg.NetworkMode == "" {
		cfg.NetworkMode = "ipv4"
	}
	if !arena.Contains([]string{"ipv4", "ipv6", "dual-stack"}, cfg.NetworkMode) {
		return fmt.Errorf("invalid network mode")
	}
	var endpoints []mdnsstress.Endpoint
	var connections []*net.UDPConn
	defer func() {
		for _, conn := range connections {
			conn.Close()
		}
	}()
	for _, family := range []string{"ipv4", "ipv6"} {
		if cfg.NetworkMode != "dual-stack" && family != cfg.NetworkMode {
			continue
		}
		endpoint, conn, err := multicastEndpoint(family)
		if err != nil {
			return err
		}
		endpoints = append(endpoints, endpoint)
		connections = append(connections, conn)
	}

	identity := make([]byte, 16)
	if _, err = rand.Read(identity); err != nil {
		return err
	}
	incarnation := hex.EncodeToString(identity)
	var out sync.Mutex
	var seq uint64
	emit := func(kind, id string, data map[string]any) {
		out.Lock()
		defer out.Unlock()
		seq++
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"type": kind, "commandId": id, "nodeId": cfg.NodeID, "incarnation": incarnation, "sequence": seq, "at": time.Now().UTC(), "data": data}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	runner := mdnsstress.NewEndpoints(endpoints, cfg.NetworkMode, func(kind string, data map[string]any) { emit(kind, "", data) })
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	defer runner.Stop()
	emit("capabilities", "", map[string]any{"protocolVersion": 1, "commands": []string{"snapshot", "mdns-stress.start", "mdns-stress.stop", "mdns-stress.status", "observe.mdns"}})
	for i, conn := range connections {
		go func(conn *net.UDPConn, family string) {
			b := make([]byte, 9001)
			for {
				n, _, err := conn.ReadFromUDP(b)
				if err != nil {
					return
				}
				runner.CaptureFamily(b[:n], family)
			}
		}(conn, endpoints[i].Family)
	}

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var c arena.Command
			if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
				emit("command.invalid", "", map[string]any{"error": err.Error()})
				continue
			}
			var err error
			switch c.Command {
			case "snapshot":
				emit("snapshot", "", map[string]any{"mdns": runner.Snapshot()})
			case "mdns-stress.status":
				emit("command.result", c.ID, map[string]any{"ok": true, "mdns": runner.Snapshot()})
				continue
			case "mdns-stress.stop":
				runner.Stop()
			case "mdns-stress.start":
				var p arena.StressProfile
				if cfg.Stress != nil {
					p = *cfg.Stress
				}
				if v, ok := c.Params["profile"]; ok {
					var b []byte
					b, err = json.Marshal(v)
					if err == nil {
						err = json.Unmarshal(b, &p)
					}
				}
				if err == nil {
					err = runner.Start(ctx, p)
				}
			default:
				err = fmt.Errorf("unsupported command")
			}
			result := map[string]any{"ok": err == nil}
			if err != nil {
				result["error"] = err.Error()
			}
			emit("command.result", c.ID, result)
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			emit("snapshot", "", map[string]any{"mdns": runner.Snapshot()})
		}
	}
}

// Internal Docker bridges have no default route. Select their IPv4 interface
// explicitly for both group membership and outbound multicast.
func multicastInterface(family string) (*net.Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && !ip.IsUnspecified() && (family == "ipv4") == (ip.To4() != nil) {
				return &iface, nil
			}
		}
	}
	return nil, fmt.Errorf("no multicast %s interface", family)
}

func multicastEndpoint(family string) (mdnsstress.Endpoint, *net.UDPConn, error) {
	iface, err := multicastInterface(family)
	if err != nil {
		return mdnsstress.Endpoint{}, nil, err
	}
	network := "udp4"
	target := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	if family == "ipv6" {
		network = "udp6"
		target = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353, Zone: iface.Name}
	}
	conn, err := net.ListenMulticastUDP(network, iface, target)
	if err != nil {
		return mdnsstress.Endpoint{}, nil, err
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		conn.Close()
		return mdnsstress.Endpoint{}, nil, err
	}
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		if family == "ipv6" {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_MULTICAST_HOPS, 255)
			if sockErr == nil {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_MULTICAST_IF, iface.Index)
			}
		} else {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_TTL, 255)
			if sockErr == nil {
				sockErr = syscall.SetsockoptIPMreqn(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_IF, &syscall.IPMreqn{Ifindex: int32(iface.Index)})
			}
		}
	})
	if err == nil {
		err = sockErr
	}
	if err != nil {
		conn.Close()
		return mdnsstress.Endpoint{}, nil, err
	}
	return mdnsstress.Endpoint{Sender: conn, Target: target, Family: family}, conn, nil
}
