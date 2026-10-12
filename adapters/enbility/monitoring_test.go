package main

import (
	"math"
	"testing"
	"time"

	gcpmgcp "github.com/enbility/eebus-go/usecases/gcp/mgcp"
	"github.com/enbility/spine-go/model"
	spimpl "github.com/enbility/spine-go/spine"
)

func TestMeasurementInput(t *testing.T) {
	for _, params := range []map[string]any{
		{"metric": "power", "value": math.NaN()},
		{
			"metric": "power",
			"value":  math.Inf(1),
		},
		{
			"metric": "power",
			"value":  1e13,
		},
		{
			"metric": "voltage",
			"value":  5.0,
		},
		{
			"metric": "energyConsumed",
			"value":  -1.0,
		},
		{
			"metric": "energyProduced",
			"value":  1.0,
		},
		{
			"metric": "power",
			"value":  "12",
		},
		{
			"metric": "power",
			"value":  12.0,
			"state":  "bad",
		},
	} {
		if _, _, _, err := validateMeasurement(params); err == nil {
			t.Fatalf("accepted %v", params)
		}
	}
	for _, state := range []string{"normal", "outOfRange", "error"} {
		if _, _, _, err := validateMeasurement(map[string]any{
			"metric": "power",
			"value":  -5.0,
			"state":  state,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeMGCPDeclaresProviderActor(t *testing.T) {
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
		model.EntityTypeTypeGridConnectionPointOfPremises,
		[]model.AddressEntityType{1},
		time.Hour,
	)
	device.AddEntity(entity)
	source := model.MeasurementValueSourceTypeCalculatedValue
	provider, err := gcpmgcp.NewMGCP(entity, nil, nil, &gcpmgcp.MonitorPowerConfig{ValueSource: &source}, &gcpmgcp.MonitorEnergyConfig{
		ValueSourceProduction:  &source,
		ValueSourceConsumption: &source,
	}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = provider
	// Read the native public declaration instead of a normalized adapter label.
	if provider.UseCaseActor != model.UseCaseActorTypeGridConnectionPoint {
		t.Fatal("native GCP provider advertises wrong actor")
	}
}
