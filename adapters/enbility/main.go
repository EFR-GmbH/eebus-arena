package main

import (
	"bufio"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/enbility/eebus-go/api"
	"github.com/enbility/eebus-go/service"
	uc "github.com/enbility/eebus-go/usecases/api"
	cslpc "github.com/enbility/eebus-go/usecases/cs/lpc"
	cslpp "github.com/enbility/eebus-go/usecases/cs/lpp"
	eglpc "github.com/enbility/eebus-go/usecases/eg/lpc"
	eglpp "github.com/enbility/eebus-go/usecases/eg/lpp"
	ship "github.com/enbility/ship-go/api"
	"github.com/enbility/ship-go/mdns"
	spine "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
	spimpl "github.com/enbility/spine-go/spine"
)

type config struct {
	SchemaVersion  int      `json:"schemaVersion"`
	NodeID         string   `json:"nodeId"`
	Role           string   `json:"role"`
	Monitoring     string   `json:"monitoring"`
	NetworkMode    string   `json:"networkMode"`
	TrustedSKIs    []string `json:"trustedSkis"`
	Certificate    string   `json:"certificate"`
	PrivateKey     string   `json:"privateKey"`
	Heartbeat      float64  `json:"heartbeatIntervalSeconds"`
	TimeScale      int      `json:"timeScale"`
	Port           int      `json:"port"`
	Interfaces     []string `json:"interfaces,omitempty"`
	StateDirectory string   `json:"stateDirectory"`
	PairingRole    string   `json:"pairingRole"`
	Pairing        struct {
		Secret string `json:"secret"`
	} `json:"pairing"`
}
type command struct {
	ID      string         `json:"id"`
	Command string         `json:"command"`
	Params  map[string]any `json:"params"`
}
type adapter struct {
	pairing                     *pairingStore
	traceMu                     sync.Mutex
	traceWindow                 time.Time
	traceCount, traceSuppressed uint64
	traceTotals                 map[string]uint64
	cfg                         config
	service                     *service.Service
	entity                      spine.EntityLocalInterface
	csLPC                       uc.CsLPCInterface
	csLPP                       uc.CsLPPInterface
	egLPC                       uc.EgLPCInterface
	egLPP                       uc.EgLPPInterface
	gcpMGCP                     uc.GcpMGCPInterface
	muMPC                       uc.MuMPCInterface
	maMGCP                      uc.MaMGCPInterface
	maMPC                       uc.MaMPCInterface
	mu                          sync.Mutex
	out                         sync.Mutex
	sequence                    uint64
	incarnation                 string
	peers                       map[string]string
	remotes                     map[string]spine.EntityRemoteInterface
}

// Native callbacks and the command loop share stdout. Keep sequence allocation
// and JSON encoding under one lock so event order matches sequence order.
func (a *adapter) emit(kind, commandID string, data any) {
	a.out.Lock()
	defer a.out.Unlock()
	a.sequence++
	if data == nil {
		data = map[string]any{}
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type":        kind,
		"commandId":   commandID,
		"data":        data,
		"nodeId":      a.cfg.NodeID,
		"at":          time.Now().UTC(),
		"sequence":    a.sequence,
		"incarnation": a.incarnation,
	})
}

func (a *adapter) result(c command, err error, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["ok"] = err == nil
	if err != nil {
		data["error"] = err.Error()
	}
	a.emit("command.result", c.ID, data)
}

func value(m map[string]any, key string, def float64) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return def
}

func validateFailsafe(params map[string]any) error {
	_, hasValue := params["value"].(float64)
	seconds, hasDuration := params["durationSeconds"].(float64)
	if _, present := params["value"]; present && !hasValue {
		return fmt.Errorf("value must be a number")
	}
	if _, present := params["durationSeconds"]; present && !hasDuration {
		return fmt.Errorf("durationSeconds must be a number")
	}
	if !hasValue && !hasDuration {
		return fmt.Errorf("value or durationSeconds is required")
	}
	if hasDuration && (seconds < 7200 || seconds > 86400) {
		return fmt.Errorf("failsafe duration must be 7200–86400 seconds")
	}
	return nil
}

func (a *adapter) run() error {
	if a.cfg.NetworkMode == "" {
		a.cfg.NetworkMode = "ipv4"
	}
	if a.cfg.NetworkMode != "ipv4" && a.cfg.NetworkMode != "ipv6" && a.cfg.NetworkMode != "dual-stack" {
		return fmt.Errorf("invalid network mode")
	}
	if !arenaMonitoringSelection(a.cfg.Monitoring) || (a.cfg.Monitoring == "mgcp" && a.cfg.Role != "EG") {
		return fmt.Errorf("invalid monitoring provider for role")
	}
	ship.ArenaObserver = a.trace
	certificate, err := tls.LoadX509KeyPair(a.cfg.Certificate, a.cfg.PrivateKey)
	if err != nil {
		return err
	}
	// CEM is a compatible CS entity for both LPC and LPP in the pinned stack.
	entityType := model.EntityTypeTypeCEM
	deviceType := model.DeviceTypeTypeGeneric
	category := ship.DeviceCategoryTypeHVAC
	if a.cfg.Role == "EG" {
		entityType = model.EntityTypeTypeGridGuard
		deviceType = model.DeviceTypeTypeElectricitySupplySystem
		category = ship.DeviceCategoryTypeGridConnectionHub
	}
	var pairingConfig *ship.PairingConfig
	var persistence ship.RingBufferPersistence
	if a.cfg.PairingRole != "" {
		a.pairing, err = openPairingStore(a.cfg.StateDirectory)
		if err != nil {
			return err
		}
		persistence = a.pairing
		mode := ship.PairingModeAnnouncer
		var secret []byte
		if a.cfg.PairingRole == "listener" {
			mode = ship.PairingModeListener
			secret, err = hex.DecodeString(a.cfg.Pairing.Secret)
			if err != nil || len(secret) != 16 {
				return fmt.Errorf("invalid pairing secret")
			}
		}
		pairingConfig = ship.NewPairingConfig(mode, secret)
		ship.ArenaObserver = a.trace
	}
	entities := []model.EntityTypeType{entityType}
	if entityType != model.EntityTypeTypeCEM {
		entities = append(entities, model.EntityTypeTypeCEM)
	}
	if a.cfg.Monitoring == "mgcp" {
		entities = append(entities, model.EntityTypeTypeGridConnectionPointOfPremises)
	}
	if a.cfg.Monitoring == "mpc" {
		entities = append(entities, model.EntityTypeTypeSubMeterElectricity)
	}
	ship.ArenaNetworkMode = a.cfg.NetworkMode
	cfg, err := api.NewConfiguration(
		"Arena",
		"Arena",
		a.cfg.Role,
		a.cfg.NodeID,
		[]ship.DeviceCategoryType{category},
		deviceType,
		entities,
		a.cfg.Port,
		certificate,
		time.Duration(a.cfg.Heartbeat*float64(time.Second)),
		pairingConfig,
		persistence,
	)
	if err != nil {
		return err
	}
	cfg.SetAlternateIdentifier("Arena-" + a.cfg.NodeID)
	if len(a.cfg.Interfaces) > 0 {
		cfg.SetInterfaces(a.cfg.Interfaces)
	}
	cfg.SetAlternateMdnsServiceName(a.cfg.NodeID)
	cfg.SetMdnsProviderSelection(mdns.MdnsProviderSelectionGoZeroConfOnly)
	a.service = service.NewService(cfg, a)
	a.service.SetLogging(a)
	if err = a.service.Setup(); err != nil {
		return err
	}
	a.service.ArenaSetMaxConnections(256)
	spimpl.ArenaMessageObserver = func(kind string, err error) {
		a.emit(kind, "", map[string]any{"error": err.Error()})
	}
	a.entity = a.service.LocalDevice().EntityForType(entityType)
	if a.cfg.Role == "EG" {
		a.egLPC = eglpc.NewLPC(a.entity, a.onLPC)
		a.egLPP = eglpp.NewLPP(a.entity, a.onLPP)
		err = a.service.AddUseCase(a.egLPC)
		if err == nil {
			err = a.service.AddUseCase(a.egLPP)
		}
	} else {
		a.csLPC = cslpc.NewLPC(a.entity, a.onLPC)
		a.csLPP = cslpp.NewLPP(a.entity, a.onLPP)
		err = a.service.AddUseCase(a.csLPC)
		if err == nil {
			err = a.service.AddUseCase(a.csLPP)
		}
		if err == nil {
			err = a.csLPC.SetConsumptionNominalMax(10000)
		}
		if err == nil {
			err = a.csLPP.SetProductionNominalMax(10000)
		}
		a.csLPC.SetFailsafeConsumptionActivePowerLimit(1000, true)
		a.csLPP.SetFailsafeProductionActivePowerLimit(1000, true)
		a.csLPC.SetFailsafeDurationMinimum(2*time.Hour, true)
		a.csLPP.SetFailsafeDurationMinimum(2*time.Hour, true)
	}
	if err != nil {
		return err
	}
	if err = a.setupMonitoring(); err != nil {
		return err
	}
	for _, ski := range a.cfg.TrustedSKIs {
		a.service.RegisterRemoteService(ship.NewServiceIdentity(ski, "", ""))
	}
	if a.pairing != nil {
		for _, i := range a.pairing.identities() {
			a.service.RegisterRemoteService(i)
		}
	}
	if err = a.service.Start(); err != nil {
		return err
	}
	caps := []string{
		"timing.accelerated",
		"mdns.query",
		"snapshot",
		"heartbeat.set",
		"heartbeat.interval",
		"observe.limit",
		"observe.ship",
		"observe.mdns",
		"observe.spine",
		"observe.partners",
	}
	caps = append(caps, "measurement.read", "observe.measurement", "monitoring.mgcp", "monitoring.mpc")
	if a.gcpMGCP != nil || a.muMPC != nil {
		caps = append(caps, "measurement.set")
	}
	if a.pairing != nil {
		caps = append(caps, "pairing.status", "observe.pairing", "observe.pairing.trace")
		if a.cfg.Role == "EG" {
			caps = append(caps, "pairing.announce", "pairing.stop")
		} else {
			caps = append(caps, "pairing.listen")
		}
	}
	if a.cfg.Role == "EG" {
		caps = append(caps, "limit.set", "failsafe.set", "constraints.read")
	}
	a.emit("capabilities", "", map[string]any{
		"protocolVersion": 1,
		"commands":        caps,
		"gaps":            []string{"upstream does not provide a complete LPC/LPP CS state machine"},
	})
	return nil
}

// Copy callback-owned collections before querying native state. Holding the
// adapter lock across a native call could deadlock on a reentrant callback.
func (a *adapter) snapshot() map[string]any {
	a.mu.Lock()
	peers := map[string]any{}
	for k, v := range a.peers {
		peers[k] = v
	}
	a.mu.Unlock()
	partners := map[string]any{"lpc": map[string]bool{}, "lpp": map[string]bool{}}
	a.mu.Lock()
	for key := range a.remotes {
		for _, uc := range []string{"lpc", "lpp"} {
			suffix := "/" + uc
			if strings.HasSuffix(key, suffix) {
				var err error
				if uc == "lpc" && a.egLPC != nil {
					_, err = a.egLPC.ConsumptionLimit(a.remotes[key])
				} else if uc == "lpp" && a.egLPP != nil {
					_, err = a.egLPP.ProductionLimit(a.remotes[key])
				} else {
					continue
				}
				partners[uc].(map[string]bool)[strings.TrimSuffix(key, suffix)] = err == nil
			}
		}
	}
	a.mu.Unlock()
	data := map[string]any{
		"peers":       peers,
		"partners":    partners,
		"stateSource": "observed",
		"timeScale":   a.cfg.TimeScale,
		"localSki":    a.service.LocalService().SKI(),
	}
	data["monitoring"] = a.monitoringSnapshot()
	data["networkMode"] = a.cfg.NetworkMode
	if a.csLPC != nil {
		if l, err := a.csLPC.ConsumptionLimit(); err == nil {
			data["lpc"] = map[string]any{
				"value":           l.Value,
				"active":          l.IsActive,
				"durationSeconds": l.Duration.Seconds(),
			}
		}
		if l, err := a.csLPP.ProductionLimit(); err == nil {
			data["lpp"] = map[string]any{
				"value":           l.Value,
				"active":          l.IsActive,
				"durationSeconds": l.Duration.Seconds(),
			}
		}
	}
	if a.pairing != nil {
		data["pairing"] = a.pairingSnapshot()
	}
	return data
}

func (a *adapter) handle(c command) {
	switch c.Command {
	case "measurement.set":
		a.setMeasurement(c)
	case "measurement.read":
		a.result(c, nil, map[string]any{"monitoring": a.monitoringSnapshot()})
	case "pairing.status", "pairing.announce", "pairing.stop":
		a.handlePairing(c)
	case "mdns.query":
		a.service.ArenaQueryMdns()
		a.result(c, nil, nil)
	case "snapshot":
		a.emit("snapshot", "", a.snapshot())
		a.result(c, nil, nil)
	case "heartbeat.set":
		enabled, ok := c.Params["enabled"].(bool)
		if !ok {
			a.result(c, fmt.Errorf("enabled must be a boolean"), nil)
			return
		}
		if enabled {
			a.result(c, a.entity.HeartbeatManager().StartHeartbeat(), nil)
		} else {
			a.entity.HeartbeatManager().StopHeartbeat()
			a.result(c, nil, nil)
		}
	case "heartbeat.interval":
		seconds := value(c.Params, "seconds", 0)
		if seconds < 1 || seconds > 3600 {
			a.result(c, fmt.Errorf("seconds must be 1–3600"), nil)
			return
		}
		manager, ok := a.entity.HeartbeatManager().(*spimpl.HeartbeatManager)
		if !ok {
			a.result(c, fmt.Errorf("heartbeat manager hook unavailable"), nil)
			return
		}
		a.result(c, manager.ArenaSetInterval(time.Duration(seconds*float64(time.Second))), nil)
	case "limit.set", "failsafe.set", "constraints.read":
		if a.egLPC == nil {
			a.result(c, fmt.Errorf("command requires EG role"), nil)
			return
		}
		ski, _ := c.Params["targetSki"].(string)
		usecase, _ := c.Params["usecase"].(string)
		if usecase != "lpc" && usecase != "lpp" {
			a.result(c, fmt.Errorf("usecase must be lpc or lpp"), nil)
			return
		}
		a.mu.Lock()
		entity := a.remotes[ski+"/"+usecase]
		a.mu.Unlock()
		if entity == nil {
			a.result(c, fmt.Errorf("use-case partner is not ready"), nil)
			return
		}
		if c.Command == "constraints.read" {
			var v float64
			var err error
			if usecase == "lpp" {
				v, err = a.egLPP.ProductionNominalMax(entity)
			} else {
				v, err = a.egLPC.ConsumptionNominalMax(entity)
			}
			a.result(c, err, map[string]any{"value": v})
			return
		}
		if c.Command == "failsafe.set" {
			if err := validateFailsafe(c.Params); err != nil {
				a.result(c, err, nil)
				return
			}
			var err error
			if v, ok := c.Params["value"].(float64); ok {
				if usecase == "lpp" {
					_, err = a.egLPP.WriteFailsafeProductionActivePowerLimit(entity, v)
				} else {
					_, err = a.egLPC.WriteFailsafeConsumptionActivePowerLimit(entity, v)
				}
			}
			if seconds, ok := c.Params["durationSeconds"].(float64); ok && err == nil {
				if seconds < 7200 || seconds > 86400 {
					err = fmt.Errorf("failsafe duration must be 7200–86400 seconds")
				} else if usecase == "lpp" {
					_, err = a.egLPP.WriteFailsafeDurationMinimum(entity, time.Duration(seconds*float64(time.Second)))
				} else {
					_, err = a.egLPC.WriteFailsafeDurationMinimum(entity, time.Duration(seconds*float64(time.Second)))
				}
			}
			a.result(c, err, map[string]any{"acceptance": "sent; use receiver observations to confirm"})
			return
		}
		v, ok := c.Params["value"].(float64)
		active, activeOK := c.Params["active"].(bool)
		duration := value(c.Params, "durationSeconds", 600)
		if !ok || !activeOK || duration < 1 || duration > 86400 {
			a.result(c, fmt.Errorf("value, active and valid durationSeconds are required"), nil)
			return
		}
		limit := uc.LoadLimit{
			Value:    v,
			IsActive: active,
			Duration: time.Duration(duration * float64(time.Second)),
		}
		started := time.Now()
		cb := func(result model.ResultDataType, counter model.MsgCounterType) {
			err := error(nil)
			if result.ErrorNumber == nil || *result.ErrorNumber != model.ErrorNumberTypeNoError {
				err = fmt.Errorf("SPINE rejected write: %v", result)
			}
			a.result(c, err, map[string]any{
				"messageCounter": counter,
				"elapsedMs":      float64(time.Since(started).Microseconds()) / 1000,
				"targetSki":      ski,
			})
			a.emit("spine.response", c.ID, map[string]any{
				"messageCounter": counter,
				"elapsedMs":      float64(time.Since(started).Microseconds()) / 1000,
				"targetSki":      ski,
				"ok":             err == nil,
			})
		}
		var counter *model.MsgCounterType
		var err error
		if usecase == "lpp" {
			counter, err = a.egLPP.WriteProductionLimit(entity, limit, cb)
		} else {
			counter, err = a.egLPC.WriteConsumptionLimit(entity, limit, cb)
		}
		if err != nil {
			a.emit("spine.send_failure", c.ID, map[string]any{"error": err.Error()})
			a.result(c, err, nil)
		} else {
			a.emit("spine.request", c.ID, map[string]any{
				"messageCounter": counter,
				"targetSki":      ski,
				"usecase":        usecase,
			})
		}
	default:
		a.result(c, fmt.Errorf("unsupported command"), nil)
	}
}

func (a *adapter) onLPC(ski string, device spine.DeviceRemoteInterface, entity spine.EntityRemoteInterface, event api.EventType) {
	a.onUC("lpc", ski, entity, event)
	if a.csLPC != nil {
		for counter := range a.csLPC.PendingConsumptionLimits() {
			a.csLPC.ApproveOrDenyConsumptionLimit(counter, true, "")
			if limit, err := a.csLPC.ConsumptionLimit(); err == nil {
				a.emit("usecase.limit", "", map[string]any{
					"usecase": "lpc",
					"value":   limit.Value,
					"active":  limit.IsActive,
				})
			}
		}
		for counter := range a.csLPC.PendingDeviceConfigurations() {
			a.csLPC.ApproveOrDenyDeviceConfiguration(counter, true, "")
		}
	}
}

func (a *adapter) onLPP(ski string, device spine.DeviceRemoteInterface, entity spine.EntityRemoteInterface, event api.EventType) {
	a.onUC("lpp", ski, entity, event)
	if a.csLPP != nil {
		for counter := range a.csLPP.PendingProductionLimits() {
			a.csLPP.ApproveOrDenyProductionLimit(counter, true, "")
			if limit, err := a.csLPP.ProductionLimit(); err == nil {
				a.emit("usecase.limit", "", map[string]any{
					"usecase": "lpp",
					"value":   limit.Value,
					"active":  limit.IsActive,
				})
			}
		}
		for counter := range a.csLPP.PendingDeviceConfigurations() {
			a.csLPP.ApproveOrDenyDeviceConfiguration(counter, true, "")
		}
	}
}

func (a *adapter) onUC(uc, ski string, entity spine.EntityRemoteInterface, event api.EventType) {
	compatible := entity != nil &&
		((uc == "lpc" && a.egLPC != nil && a.egLPC.IsCompatibleEntityType(entity)) ||
			(uc == "lpp" && a.egLPP != nil && a.egLPP.IsCompatibleEntityType(entity)))
	if compatible && a.cfg.Role == "EG" {
		a.mu.Lock()
		a.remotes[ski+"/"+uc] = entity
		a.mu.Unlock()
	}
	a.emit("usecase.event", "", map[string]any{"usecase": uc, "ski": ski, "event": event})
}

func (a *adapter) RemoteServiceConnected(_ api.ServiceInterface, i ship.ServiceIdentity) {
	a.ServiceUpdated(i)
	a.mu.Lock()
	a.peers[i.SKI] = "connected"
	a.mu.Unlock()
	a.emit("ship.state", "", map[string]any{"ski": i.SKI, "state": "connected"})
}

func (a *adapter) RemoteServiceDisconnected(_ api.ServiceInterface, i ship.ServiceIdentity) {
	a.mu.Lock()
	a.peers[i.SKI] = "disconnected"
	delete(a.remotes, i.SKI+"/lpc")
	delete(a.remotes, i.SKI+"/lpp")
	delete(a.remotes, i.SKI+"/mgcp")
	delete(a.remotes, i.SKI+"/mpc")
	a.mu.Unlock()
	a.emit("ship.state", "", map[string]any{"ski": i.SKI, "state": "disconnected"})
}

func (a *adapter) VisibleRemoteMdnsServicesUpdated(_ api.ServiceInterface, entries []ship.RemoteMdnsService) {
	a.emit("mdns.services", "", map[string]any{"count": len(entries)})
}

func (a *adapter) ServiceUpdated(i ship.ServiceIdentity) {
	if a.pairing != nil {
		for _, known := range a.pairing.identities() {
			if known.Fingerprint == i.Fingerprint {
				a.persistIdentity(i, false)
				break
			}
		}
	}
}

func (a *adapter) ServicePairingDetailUpdate(i ship.ServiceIdentity, d *ship.ConnectionStateDetail) {
	a.emit("ship.transition", "", map[string]any{
		"ski":         i.SKI,
		"shipId":      i.ShipID,
		"fingerprint": i.Fingerprint,
		"state":       d.State(),
	})
}

func (a *adapter) AllowWaitingForTrust(i ship.ServiceIdentity) bool {
	for _, ski := range a.cfg.TrustedSKIs {
		if ski == i.SKI {
			return true
		}
	}
	return false
}

func (a *adapter) ServiceAutoTrustFailed(_ api.ServiceInterface, i ship.ServiceIdentity, err error) {
	a.trace("pairing.validation", map[string]any{
		"stage":       "outcome",
		"ok":          false,
		"ski":         i.SKI,
		"shipId":      i.ShipID,
		"fingerprint": i.Fingerprint,
		"reason":      err.Error(),
	})
}

func (a *adapter) ServiceAutoTrustRemoved(_ api.ServiceInterface, i ship.ServiceIdentity, reason string) {
	a.persistIdentity(i, true)
	a.emit("pairing.trust", "", map[string]any{"identity": i, "trusted": false, "reason": reason})
}

func (a *adapter) ServiceAutoTrusted(_ api.ServiceInterface, i ship.ServiceIdentity) {
	a.persistIdentity(i, false)
	a.emit("pairing.trust", "", map[string]any{"identity": i, "trusted": true})
}

func (a *adapter) Trace(v ...interface{})            {}
func (a *adapter) Tracef(f string, v ...interface{}) {}
func (a *adapter) Debug(v ...interface{})            {}
func (a *adapter) Debugf(f string, v ...interface{}) {}
func (a *adapter) Info(v ...interface{})             { fmt.Fprintln(os.Stderr, v...) }
func (a *adapter) Infof(f string, v ...interface{})  { fmt.Fprintf(os.Stderr, f+"\n", v...) }
func (a *adapter) Error(v ...interface{}) {
	msg := fmt.Sprint(v...)
	fmt.Fprintln(os.Stderr, msg)
	a.emit("stack.error", "", map[string]any{"error": msg})
}

func (a *adapter) Errorf(f string, v ...interface{}) { a.Error(fmt.Sprintf(f, v...)) }
func main() {
	a := &adapter{
		incarnation: fmt.Sprintf("%d", time.Now().UnixNano()),
		peers:       map[string]string{},
		remotes:     map[string]spine.EntityRemoteInterface{},
	}
	configPath := os.Getenv("ARENA_CONFIG")
	if configPath == "" {
		configPath = "/identity/config.json"
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatal(err)
	}
	if err = json.Unmarshal(b, &a.cfg); err != nil {
		log.Fatal(err)
	}
	if a.cfg.TimeScale != 1 && a.cfg.TimeScale != 10 {
		log.Fatal("timeScale must be 1 or 10")
	}
	atomic.StoreInt64(&spimpl.ArenaTimeScale, int64(a.cfg.TimeScale))
	if err = a.run(); err != nil {
		log.Fatal(err)
	}
	defer a.service.Shutdown()
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var c command
			if err := json.Unmarshal(scanner.Bytes(), &c); err == nil {
				a.handle(c)
			} else {
				a.emit("command.invalid", "", map[string]any{"error": err.Error()})
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case <-sig:
			return
		case <-ticker.C:
			a.emit("snapshot", "", a.snapshot())
		}
	}
}
