package main

import (
	"github.com/enbility/spine-go/model"
	spimpl "github.com/enbility/spine-go/spine"
	"testing"
	"time"
)

func TestFailsafeRejectsNoOpAndValidatesBeforeWrites(t *testing.T) {
	for _, params := range []map[string]any{
		{},
		{"value": "1200"},
		{"durationSeconds": false},
		{
			"value":           1200.0,
			"durationSeconds": 1.0,
		},
		{"durationSeconds": 86401.0},
	} {
		if validateFailsafe(params) == nil {
			t.Fatalf("invalid failsafe accepted: %v", params)
		}
	}
	for _, params := range []map[string]any{
		{"value": 1200.0},
		{"durationSeconds": 7200.0},
		{
			"value":           1200.0,
			"durationSeconds": 86400.0,
		},
	} {
		if err := validateFailsafe(params); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntervalPreservesHeartbeatSuppression(t *testing.T) {
	device := spimpl.NewDeviceLocal(
		"brand",
		"model",
		"serial",
		"code",
		"address",
		model.DeviceTypeTypeEnergyManagementSystem,
		model.NetworkManagementFeatureSetTypeSmart,
	)
	entity := spimpl.NewEntityLocal(
		device,
		model.EntityTypeTypeCEM,
		[]model.AddressEntityType{1},
		time.Hour,
	)
	device.AddEntity(entity)
	h := entity.HeartbeatManager().(*spimpl.HeartbeatManager)
	feature := entity.GetOrAddFeature(model.FeatureTypeTypeDeviceDiagnosis, model.RoleTypeServer)
	feature.AddFunctionType(model.FunctionTypeDeviceDiagnosisHeartbeatData, true, false)
	h.SetLocalFeature(entity, feature)
	defer h.StopHeartbeat()
	if !h.IsHeartbeatRunning() {
		t.Fatal("heartbeat didn't start")
	}
	if err := h.ArenaSetInterval(time.Hour * 2); err != nil {
		t.Fatal(err)
	}
	if !h.IsHeartbeatRunning() {
		t.Fatal("interval disabled active heartbeats")
	}
	h.StopHeartbeat()
	if err := h.ArenaSetInterval(time.Hour * 3); err != nil {
		t.Fatal(err)
	}
	if h.IsHeartbeatRunning() {
		t.Fatal("interval re-enabled suppressed heartbeats")
	}
	if err := h.ArenaSetInterval(0); err == nil {
		t.Fatal("invalid interval accepted")
	}
}
