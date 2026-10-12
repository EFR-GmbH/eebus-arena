package arena

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"
)

const SchemaVersion = 3

var safeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var immutableImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Variant struct {
	BuiltAt    time.Time       `json:"builtAt,omitempty"`
	ID         string          `json:"id"`
	Stack      string          `json:"stack"`
	Label      string          `json:"label"`
	Roles      []string        `json:"roles"`
	Image      string          `json:"image"`
	Provenance json.RawMessage `json:"provenance"`
}
type Group struct {
	ID         string          `json:"id"`
	Variant    string          `json:"variant"`
	Role       string          `json:"role"`
	Count      int             `json:"count"`
	MemoryMB   int             `json:"memoryMB,omitempty"`
	Resources  *ResourceLimits `json:"resources,omitempty"`
	Stress     *StressProfile  `json:"stress,omitempty"`
	External   *ExternalNode   `json:"external,omitempty"`
	Monitoring string          `json:"monitoring,omitempty"`
}
type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
	Mode string `json:"mode,omitempty"`
}
type Definition struct {
	SchemaVersion   int                     `json:"schemaVersion"`
	Name            string                  `json:"name"`
	Seed            int64                   `json:"seed"`
	TimeScale       int                     `json:"timeScale"`
	TimingProfile   string                  `json:"timingProfile,omitempty"`
	Groups          []Group                 `json:"groups"`
	Links           []Link                  `json:"links,omitempty"`
	Scenario        string                  `json:"scenario"`
	DurationSeconds int                     `json:"durationSeconds,omitempty"`
	Steps           []Step                  `json:"steps,omitempty"`
	NodeOverrides   map[string]NodeOverride `json:"nodeOverrides,omitempty"`
	Sampling        *SamplingProfile        `json:"sampling,omitempty"`
	Campaign        *Campaign               `json:"campaign,omitempty"`
	Network         *LabNetwork             `json:"network,omitempty"`
	Exploration     *Exploration            `json:"exploration,omitempty"`
}
type Step struct {
	Name     string               `json:"name"`
	Target   string               `json:"target,omitempty"`
	Action   string               `json:"action"`
	Command  string               `json:"command,omitempty"`
	Params   map[string]any       `json:"params,omitempty"`
	Seconds  int                  `json:"seconds,omitempty"`
	Expect   *Expectation         `json:"expect,omitempty"`
	Fault    *NetworkFault        `json:"fault,omitempty"`
	Resource *ResourceExpectation `json:"resource,omitempty"`
}
type Expectation struct {
	Target        string         `json:"target,omitempty"`
	Path          []string       `json:"path"`
	Equals        any            `json:"equals"`
	WithinSeconds int            `json:"withinSeconds"`
	Event         string         `json:"event,omitempty"`
	Match         map[string]any `json:"match,omitempty"`
	Absent        bool           `json:"absent,omitempty"`
	RealTime      bool           `json:"realTime,omitempty"`
}
type Identity struct {
	SKI         string `json:"ski"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}
type Bundle struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Definition    Definition                   `json:"definition"`
	Identities    map[string]Identity          `json:"identities,omitempty"`
	Variants      []Variant                    `json:"variants,omitempty"`
	Pairing       map[string]PairingCredential `json:"pairing,omitempty"`
}
type Node struct {
	PairingProgress       map[string]*PairingProgress `json:"pairingProgress,omitempty"`
	PairingMetrics        map[string]*PairingMetric   `json:"pairingMetrics,omitempty"`
	action                uint64
	ID                    string                 `json:"id"`
	Group                 string                 `json:"group"`
	Variant               string                 `json:"variant"`
	Stack                 string                 `json:"stack"`
	Role                  string                 `json:"role"`
	Monitoring            string                 `json:"monitoring,omitempty"`
	MonitoringObservation *MonitoringObservation `json:"monitoringObservation,omitempty"`
	SKI                   string                 `json:"ski"`
	TrustedSKIs           []string               `json:"trustedSkis"`
	IntendedPeers         []string               `json:"intendedPeers,omitempty"`
	PairingPeers          []string               `json:"pairingPeers,omitempty"`
	ShipID                string                 `json:"shipId,omitempty"`
	Fingerprint           string                 `json:"fingerprint,omitempty"`
	Container             string                 `json:"container,omitempty"`
	Lifecycle             string                 `json:"lifecycle"`
	Capabilities          []string               `json:"capabilities"`
	Snapshot              map[string]any         `json:"snapshot"`
	SnapshotAt            time.Time              `json:"snapshotAt"`
	Metrics               map[string]any         `json:"metrics"`
	Resources             ResourceLimits         `json:"resources"`
	External              *ExternalNode          `json:"external,omitempty"`
	EffectiveResources    map[string]any         `json:"effectiveResources,omitempty"`
	ProcessMetrics        map[string]any         `json:"processMetrics,omitempty"`
	ResourceIncarnation   string                 `json:"resourceIncarnation,omitempty"`
	ResourceSequence      uint64                 `json:"resourceSequence,omitempty"`
	ResourceAt            time.Time              `json:"resourceAt,omitempty"`
	LastSeen              time.Time              `json:"lastSeen"`
	Incarnation           string                 `json:"incarnation,omitempty"`
	Sequence              uint64                 `json:"sequence"`
	Counters              map[string]uint64      `json:"counters,omitempty"`
	LogRows               map[string]int         `json:"logRows,omitempty"`
	LogAt                 time.Time              `json:"logAt,omitempty"`
	AdapterAt             time.Time              `json:"adapterAt,omitempty"`
	Retired               map[string]bool        `json:"retiredIncarnations,omitempty"`
	LimitAt               map[string]time.Time   `json:"limitAt,omitempty"`
}
type Assertion struct {
	ID        string    `json:"id"`
	Node      string    `json:"node,omitempty"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail"`
	Reference string    `json:"reference,omitempty"`
	At        time.Time `json:"at"`
}
type Run struct {
	SetupRevision      uint64         `json:"setupRevision,omitempty"`
	ID                 string         `json:"id"`
	Definition         Definition     `json:"definition"`
	Variants           []Variant      `json:"variants"`
	Status             string         `json:"status"`
	Created            time.Time      `json:"created"`
	Nodes              []*Node        `json:"nodes"`
	Assertions         []Assertion    `json:"assertions"`
	Error              string         `json:"error,omitempty"`
	Runtime            map[string]any `json:"runtime"`
	MonitoringSequence uint64         `json:"monitoringSequence,omitempty"`
	generation         uint64
}
type Command struct {
	ID      string         `json:"id"`
	Command string         `json:"command"`
	Params  map[string]any `json:"params,omitempty"`
}
type Event struct {
	Epoch           string         `json:"epoch,omitempty"`
	Index           uint64         `json:"index,omitempty"`
	At              time.Time      `json:"at"`
	ReceivedAt      time.Time      `json:"receivedAt,omitempty"`
	SourceAt        *time.Time     `json:"sourceAt,omitempty"`
	ProtocolSeconds *float64       `json:"protocolSeconds,omitempty"`
	RunID           string         `json:"runId,omitempty"`
	NodeID          string         `json:"nodeId,omitempty"`
	Incarnation     string         `json:"incarnation,omitempty"`
	Sequence        uint64         `json:"sequence,omitempty"`
	Type            string         `json:"type"`
	CommandID       string         `json:"commandId,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
}

func Contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func (d *Definition) Validate(variants map[string]Variant) error {
	legacy := d.SchemaVersion == 1 || d.SchemaVersion == 2
	if d.SchemaVersion == 1 {
		for _, l := range d.Links {
			if l.Mode != "" && l.Mode != "pretrusted" {
				return errors.New("pairing links require schema version 2")
			}
		}
		d.SchemaVersion = SchemaVersion
	}
	if d.SchemaVersion == 2 {
		d.SchemaVersion = SchemaVersion
	}
	if d.SchemaVersion != SchemaVersion {
		return errors.New("unsupported arena schema version")
	}
	if d.TimeScale == 0 {
		d.TimeScale = 1
	}
	if d.TimeScale != 1 && d.TimeScale != 2 && d.TimeScale != 5 && d.TimeScale != 10 {
		return errors.New("timeScale must be 1, 2, 5 or 10")
	}
	if legacy && d.TimeScale == 10 && d.TimingProfile == "" {
		d.TimingProfile = "legacy-partial"
	}
	if !Contains([]string{"", "qualified", "legacy-partial"}, d.TimingProfile) {
		return errors.New("unknown timing profile")
	}
	if d.Name == "" || len(d.Name) > 120 {
		return errors.New("name must contain 1–120 characters")
	}
	if d.Scenario == "" {
		d.Scenario = "manual"
	}
	if !Contains([]string{"manual", "discovery", "lpc", "lpp", "heartbeat", "restart", "stress", "custom", "pairing", "mdns-stress", "campaign", "explore", "monitoring"}, d.Scenario) {
		return errors.New("unknown scenario")
	}
	if d.DurationSeconds == 0 {
		d.DurationSeconds = 1800
	}
	if d.DurationSeconds < 1 || d.DurationSeconds > 86400 {
		return errors.New("durationSeconds must be 1–86400")
	}
	ids := map[string]Group{}
	total, guards := 0, 0
	for _, g := range d.Groups {
		if !safeID.MatchString(g.ID) {
			return fmt.Errorf("invalid group id %q", g.ID)
		}
		if _, exists := ids[g.ID]; exists {
			return fmt.Errorf("duplicate group id %q", g.ID)
		}
		ids[g.ID] = g
		v, ok := variants[g.Variant]
		if !ok {
			return fmt.Errorf("unknown variant %q", g.Variant)
		}
		if !Contains(v.Roles, g.Role) {
			return fmt.Errorf("variant %s does not support %s", g.Variant, g.Role)
		}
		if g.External != nil && v.Stack != "enbility" {
			return errors.New("managed physical stub requires the native enbility variant")
		}
		if g.Count < 1 || g.Count > 200 {
			return errors.New("group counts must be 1–200")
		}
		if g.MemoryMB != 0 && (g.MemoryMB < 64 || g.MemoryMB > 8192) {
			return errors.New("memoryMB must be 64–8192 or omitted")
		}
		if err := validateGroupResources(g); err != nil {
			return fmt.Errorf("group %s: %w", g.ID, err)
		}
		if g.Role == "STRESSER" {
			if v.Stack != "mdns-stresser" {
				return errors.New("STRESSER requires the mdns-stresser image")
			}
			if g.Stress != nil {
				if err := g.Stress.Validate(); err != nil {
					return err
				}
				if family := g.Stress.Family; family != "" && d.networkMode() != "dual-stack" && family != d.networkMode() {
					return errors.New("requested mDNS family is not enabled in this arena")
				}
			}
		} else if g.Stress != nil {
			return errors.New("stress profile requires a STRESSER node")
		}
		total += g.Count
		if g.Role == "EG" {
			guards += g.Count
		}
	}
	if total < 2 || total > 201 || guards < 1 {
		return errors.New("an arena needs 2–201 nodes including an EG")
	}
	if err := d.validateResourceSettings(); err != nil {
		return err
	}
	if err := d.validateMonitoring(variants); err != nil {
		return err
	}
	if err := d.validatePhysical(); err != nil {
		return err
	}
	if d.networkMode() != "ipv4" {
		for _, group := range d.Groups {
			if !Contains(featuresFor(variants[group.Variant]).NetworkModes, d.networkMode()) {
				return fmt.Errorf("variant %s needs rebuilding for %s configuration", group.Variant, d.networkMode())
			}
		}
	}
	if len(d.Links) == 0 {
		if guards != 1 {
			return errors.New("multiple EGs require explicit group links")
		}
		var eg string
		for _, g := range d.Groups {
			if g.Role == "EG" {
				eg = g.ID
			}
		}
		for _, g := range d.Groups {
			if g.Role == "CS" {
				d.Links = append(d.Links, Link{From: eg, To: g.ID})
			}
		}
	}
	hasPairing := false
	seen := map[[2]string]bool{}
	for _, l := range d.Links {
		a, okA := ids[l.From]
		b, okB := ids[l.To]
		if !okA || !okB || a.Role != "EG" || b.Role != "CS" || seen[[2]string{l.From, l.To}] {
			return errors.New("links must be unique EG-group to CS-group pairs")
		}
		seen[[2]string{l.From, l.To}] = true
		if l.Mode != "" && l.Mode != "pretrusted" && l.Mode != "ship-pairing" {
			return errors.New("link mode must be pretrusted or ship-pairing")
		}
		hasPairing = hasPairing || l.Mode == "ship-pairing"
		if l.Mode == "ship-pairing" && d.TimeScale != 1 {
			return errors.New("SHIP pairing requires real-time timing")
		}
	}
	if d.Scenario == "pairing" && !hasPairing {
		return errors.New("pairing scenario requires a ship-pairing link")
	}
	if d.Scenario == "mdns-stress" {
		hasStresser := false
		for _, g := range d.Groups {
			hasStresser = hasStresser || g.Role == "STRESSER"
		}
		if !hasStresser || d.TimeScale != 1 {
			return errors.New("mdns-stress requires a stresser and real-time timing")
		}
	}
	for _, g := range d.Groups {
		if g.Role == "STRESSER" {
			continue
		}
		linked := false
		for _, l := range d.Links {
			if l.From == g.ID || l.To == g.ID {
				linked = true
			}
		}
		if !linked {
			return fmt.Errorf("group %s has no trust relationship", g.ID)
		}
	}
	if len(d.Steps) > 1000 || d.Scenario == "custom" && len(d.Steps) == 0 {
		return errors.New("custom scenarios require 1–1000 steps")
	}
	for _, s := range d.Steps {
		if s.Name == "" || len(s.Name) > 120 || !Contains([]string{"wait", "command", "lifecycle", "assert", "fault", "resource"}, s.Action) {
			return errors.New("step needs a name and a known action")
		}
		if s.Action == "wait" {
			if s.Seconds < 1 || s.Seconds > 86400 {
				return errors.New("wait seconds must be 1–86400")
			}
			continue
		}
		valid := false
		for _, g := range d.Groups {
			if s.Target == g.ID {
				valid = true
			}
			for i := 1; i <= g.Count; i++ {
				if s.Target == fmt.Sprintf("%s-%03d", g.ID, i) {
					valid = true
				}
			}
		}
		if !valid {
			return fmt.Errorf("step %s has an unknown target", s.Name)
		}
		if s.Action == "fault" {
			if s.Fault == nil {
				return errors.New("fault step needs a profile")
			}
			if err := s.Fault.Validate(); err != nil {
				return err
			}
			if s.Target != s.Fault.From || !d.hasNode(s.Fault.From) || !d.hasNode(s.Fault.To) {
				return errors.New("fault step targets must be explicit existing node IDs")
			}
		}
		if s.Action == "resource" {
			if s.Resource == nil {
				return errors.New("resource step needs an expectation")
			}
			if err := s.Resource.Validate(); err != nil {
				return err
			}
		}
		if s.Action == "lifecycle" && !Contains([]string{"start", "stop", "kill", "restart", "pause", "resume"}, s.Command) {
			return errors.New("unknown lifecycle step")
		}
		if s.Action == "command" && (s.Command == "" || len(s.Command) > 80) {
			return errors.New("command step needs a command")
		}
		if s.Action == "assert" && s.Expect == nil {
			return errors.New("assert step needs an expectation")
		}
		if s.Expect != nil && s.Expect.Target != "" {
			found := false
			for _, g := range d.Groups {
				for i := 1; i <= g.Count; i++ {
					if s.Expect.Target == fmt.Sprintf("%s-%03d", g.ID, i) {
						found = true
					}
				}
			}
			if !found {
				return errors.New("unknown expectation target")
			}
		}
		if e := s.Expect; e != nil && ((len(e.Path) == 0 && e.Event == "") || (len(e.Path) > 0 && e.Event != "") || len(e.Path) > 10 || e.WithinSeconds < 1 || e.WithinSeconds > 86400 || (e.Absent && e.Event == "")) {
			return errors.New("expectation needs a snapshot path and 1–86400 second deadline")
		}
	}
	if err := d.validateCampaign(variants); err != nil {
		return err
	}
	return d.validateExploration(variants)
}

func NewIdentity(name string) (Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		return Identity{}, err
	}
	ski := sha1.Sum(pub.Bytes()) // SHIP's specified SKI derivation.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Identity{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name, Organization: []string{"EEBUS Arena"}, OrganizationalUnit: []string{"Test"}, Country: []string{"DE"}}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), SubjectKeyId: ski[:], KeyUsage: x509.KeyUsageDigitalSignature, SignatureAlgorithm: x509.ECDSAWithSHA256, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return Identity{}, err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Identity{}, err
	}
	return Identity{hex.EncodeToString(ski[:]), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}))}, nil
}
func (i Identity) Validate() error {
	cert, err := tls.X509KeyPair([]byte(i.Certificate), []byte(i.PrivateKey))
	if err != nil {
		return err
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	pub, ok := parsed.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return errors.New("identity must use ECDSA P-256")
	}
	ecdh, err := pub.ECDH()
	if err != nil {
		return err
	}
	ski := sha1.Sum(ecdh.Bytes())
	expected := hex.EncodeToString(ski[:])
	if i.SKI != expected || hex.EncodeToString(parsed.SubjectKeyId) != expected {
		return errors.New("certificate SKI does not match public key")
	}
	if time.Now().Before(parsed.NotBefore) || time.Now().After(parsed.NotAfter) {
		return errors.New("certificate is outside its validity period")
	}
	return parsed.CheckSignature(parsed.SignatureAlgorithm, parsed.RawTBSCertificate, parsed.Signature)
}

func Resolve(bundle *Bundle, variants map[string]Variant) ([]*Node, error) {
	if bundle.SchemaVersion == 1 || bundle.SchemaVersion == 2 {
		bundle.SchemaVersion = SchemaVersion
	}
	if bundle.SchemaVersion != SchemaVersion {
		return nil, errors.New("unsupported bundle schema version")
	}
	if err := bundle.Definition.Validate(variants); err != nil {
		return nil, err
	}
	if bundle.Identities == nil {
		bundle.Identities = map[string]Identity{}
	}
	nodes := []*Node{}
	skis := map[string]bool{}
	expected := map[string]bool{}
	used := map[string]bool{}
	for _, g := range bundle.Definition.Groups {
		for n := 1; n <= g.Count; n++ {
			id := fmt.Sprintf("%s-%03d", g.ID, n)
			if g.Role == "STRESSER" {
				nodes = append(nodes, &Node{ID: id, Group: g.ID, Variant: g.Variant, Stack: variants[g.Variant].Stack, Role: g.Role, Lifecycle: "created", TrustedSKIs: []string{}, Capabilities: []string{}, Snapshot: map[string]any{}, Metrics: map[string]any{}})
				used[g.Variant] = true
				continue
			}
			expected[id] = true
			identity, ok := bundle.Identities[id]
			if !ok {
				var err error
				identity, err = NewIdentity(id)
				if err != nil {
					return nil, err
				}
				bundle.Identities[id] = identity
			}
			if err := identity.Validate(); err != nil {
				return nil, fmt.Errorf("identity %s: %w", id, err)
			}
			if skis[identity.SKI] {
				return nil, errors.New("duplicate SKI in arena")
			}
			skis[identity.SKI] = true
			nodes = append(nodes, &Node{ID: id, Group: g.ID, Variant: g.Variant, Stack: variants[g.Variant].Stack, Role: g.Role, SKI: identity.SKI, TrustedSKIs: []string{}, Lifecycle: "created", Capabilities: []string{}, Snapshot: map[string]any{}, Metrics: map[string]any{}})
			used[g.Variant] = true
		}
	}
	for id := range bundle.Identities {
		if !expected[id] {
			return nil, fmt.Errorf("identity %s has no node", id)
		}
	}
	for _, n := range nodes {
		n.Resources = bundle.Definition.resourcesFor(n.ID, n.Group)
		n.Monitoring = bundle.Definition.monitoringFor(n.ID, n.Group)
		for _, group := range bundle.Definition.Groups {
			if group.ID == n.Group {
				n.External = group.External
			}
		}
	}
	for _, l := range bundle.Definition.Links {
		for _, a := range nodes {
			if a.Group != l.From {
				continue
			}
			for _, b := range nodes {
				if b.Group == l.To {
					a.IntendedPeers = append(a.IntendedPeers, b.ID)
					b.IntendedPeers = append(b.IntendedPeers, a.ID)
					if l.Mode == "ship-pairing" {
						a.PairingPeers = append(a.PairingPeers, b.ID)
						b.PairingPeers = append(b.PairingPeers, a.ID)
					} else {
						a.TrustedSKIs = append(a.TrustedSKIs, b.SKI)
						b.TrustedSKIs = append(b.TrustedSKIs, a.SKI)
					}
				}
			}
		}
	}
	if err := provisionPairing(bundle, nodes); err != nil {
		return nil, err
	}
	bundle.Variants = nil
	keys := []string{}
	for k := range used {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		bundle.Variants = append(bundle.Variants, variants[k])
	}
	return nodes, nil
}
