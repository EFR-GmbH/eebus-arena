package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/enbility/eebus-go/api"
	gcpmgcp "github.com/enbility/eebus-go/usecases/gcp/mgcp"
	mamgcp "github.com/enbility/eebus-go/usecases/ma/mgcp"
	mampc "github.com/enbility/eebus-go/usecases/ma/mpc"
	mumpc "github.com/enbility/eebus-go/usecases/mu/mpc"
	spine "github.com/enbility/spine-go/api"
	"github.com/enbility/spine-go/model"
)

func (a *adapter) setupMonitoring() error {
	consumer := a.service.LocalDevice().EntityForType(model.EntityTypeTypeCEM)
	a.maMGCP = mamgcp.NewMGCP(consumer, func(ski string, _ spine.DeviceRemoteInterface, e spine.EntityRemoteInterface, event api.EventType) {
		a.onMonitoring("mgcp", ski, e, event)
	})
	a.maMPC = mampc.NewMPC(consumer, func(ski string, _ spine.DeviceRemoteInterface, e spine.EntityRemoteInterface, event api.EventType) {
		a.onMonitoring("mpc", ski, e, event)
	})
	if err := a.service.AddUseCase(a.maMGCP); err != nil {
		return err
	}
	if err := a.service.AddUseCase(a.maMPC); err != nil {
		return err
	}
	// Controlled test readings are calculated values, separate from resource telemetry.
	source := model.MeasurementValueSourceTypeCalculatedValue
	if a.cfg.Monitoring == "mgcp" {
		entity := a.service.LocalDevice().EntityForType(model.EntityTypeTypeGridConnectionPointOfPremises)
		provider, err := gcpmgcp.NewMGCP(
			entity, nil, nil,
			&gcpmgcp.MonitorPowerConfig{ValueSource: &source},
			&gcpmgcp.MonitorEnergyConfig{
				ValueSourceProduction:  &source,
				ValueSourceConsumption: &source,
			},
			nil, nil, nil,
		)
		if err != nil {
			return err
		}
		a.gcpMGCP = provider
		return a.service.AddUseCase(provider)
	}
	if a.cfg.Monitoring == "mpc" {
		entity := a.service.LocalDevice().EntityForType(model.EntityTypeTypeSubMeterElectricity)
		provider, err := mumpc.NewMPC(
			entity, nil,
			&mumpc.MonitorPowerConfig{
				ConnectedPhases:  model.ElectricalConnectionPhaseNameTypeAbc,
				ValueSourceTotal: &source,
			},
			&mumpc.MonitorEnergyConfig{
				ValueSourceProduction:  &source,
				ValueSourceConsumption: &source,
			},
			nil, nil, nil,
		)
		if err != nil {
			return err
		}
		a.muMPC = provider
		return a.service.AddUseCase(provider)
	}
	return nil
}

func (a *adapter) onMonitoring(usecase, ski string, entity spine.EntityRemoteInterface, event api.EventType) {
	compatible := entity != nil &&
		((usecase == "mgcp" && a.maMGCP.IsCompatibleEntityType(entity)) ||
			(usecase == "mpc" && a.maMPC.IsCompatibleEntityType(entity)))
	if compatible {
		a.mu.Lock()
		a.remotes[ski+"/"+usecase] = entity
		a.mu.Unlock()
	}
	a.emit("usecase.event", "", map[string]any{"usecase": usecase, "ski": ski, "event": event})
}

func reading(value float64, err error, unit string) map[string]any {
	result := map[string]any{"available": err == nil, "unit": unit, "source": "native"}
	if err != nil {
		result["error"] = err.Error()
	} else {
		result["value"] = value
	}
	return result
}

func (a *adapter) monitoringSnapshot() map[string]any {
	local := map[string]any{}
	if a.gcpMGCP != nil {
		v, e := a.gcpMGCP.PowerTotal()
		local["power"] = reading(v, e, "W")
		v, e = a.gcpMGCP.EnergyConsumed()
		local["energyConsumed"] = reading(v, e, "Wh")
		v, e = a.gcpMGCP.EnergyFeedIn()
		local["energyProduced"] = reading(v, e, "Wh")
	} else if a.muMPC != nil {
		v, e := a.muMPC.Power()
		local["power"] = reading(v, e, "W")
		v, e = a.muMPC.EnergyConsumed()
		local["energyConsumed"] = reading(v, e, "Wh")
		v, e = a.muMPC.EnergyProduced()
		local["energyProduced"] = reading(v, e, "Wh")
	}
	// Native reads can invoke callbacks that need a.mu; snapshot the lookup table first.
	a.mu.Lock()
	remotes := map[string]spine.EntityRemoteInterface{}
	for key, entity := range a.remotes {
		remotes[key] = entity
	}
	a.mu.Unlock()
	remote := map[string]any{}
	for key, entity := range remotes {
		index := strings.LastIndex(key, "/")
		if index < 0 {
			continue
		}
		usecase, ski := key[index+1:], key[:index]
		var values map[string]any
		if usecase == "mgcp" && a.maMGCP != nil {
			values = map[string]any{}
			v, e := a.maMGCP.Power(entity)
			values["power"] = reading(v, e, "W")
			v, e = a.maMGCP.EnergyConsumed(entity)
			values["energyConsumed"] = reading(v, e, "Wh")
			v, e = a.maMGCP.EnergyFeedIn(entity)
			values["energyProduced"] = reading(v, e, "Wh")
		} else if usecase == "mpc" && a.maMPC != nil {
			values = map[string]any{}
			v, e := a.maMPC.Power(entity)
			values["power"] = reading(v, e, "W")
			v, e = a.maMPC.EnergyConsumed(entity)
			values["energyConsumed"] = reading(v, e, "Wh")
			v, e = a.maMPC.EnergyProduced(entity)
			values["energyProduced"] = reading(v, e, "Wh")
		}
		if values != nil {
			if remote[ski] == nil {
				remote[ski] = map[string]any{}
			}
			remote[ski].(map[string]any)[usecase] = values
		}
	}
	return map[string]any{"offering": a.cfg.Monitoring, "local": local, "remote": remote}
}

func validateMeasurement(params map[string]any) (string, float64, model.MeasurementValueStateType, error) {
	metric, _ := params["metric"].(string)
	v, ok := params["value"].(float64)
	state := model.MeasurementValueStateTypeNormal
	if requested, present := params["state"]; present {
		s, ok := requested.(string)
		if !ok {
			return "", 0, state, fmt.Errorf("state must be a string")
		}
		state = model.MeasurementValueStateType(s)
	}
	validMetric := metric == "power" || metric == "energyConsumed" || metric == "energyProduced"
	validState := state == model.MeasurementValueStateTypeNormal ||
		state == model.MeasurementValueStateTypeOutofrange ||
		state == model.MeasurementValueStateTypeError
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e12 || !validMetric || !validState {
		return "", 0, state, fmt.Errorf("measurement needs a known metric, finite bounded value and valid state")
	}
	if metric == "energyConsumed" && v < 0 || metric == "energyProduced" && v > 0 {
		return "", 0, state, fmt.Errorf("consumed energy is positive; produced/feed-in energy is negative")
	}
	return metric, v, state, nil
}

func (a *adapter) setMeasurement(c command) {
	metric, v, state, err := validateMeasurement(c.Params)
	if err == nil && a.gcpMGCP == nil && a.muMPC == nil {
		err = fmt.Errorf("node does not offer a monitoring use case")
	}
	if err == nil {
		now := time.Now().UTC()
		if a.gcpMGCP != nil {
			switch metric {
			case "power":
				err = a.gcpMGCP.Update(a.gcpMGCP.UpdateDataPowerTotal(v, &now, &state))
			case "energyConsumed":
				err = a.gcpMGCP.Update(a.gcpMGCP.UpdateDataEnergyConsumed(v, &now, &state, nil, nil))
			case "energyProduced":
				err = a.gcpMGCP.Update(a.gcpMGCP.UpdateDataEnergyFeedIn(v, &now, &state, nil, nil))
			}
		} else {
			switch metric {
			case "power":
				err = a.muMPC.Update(a.muMPC.UpdateDataPowerTotal(v, &now, &state))
			case "energyConsumed":
				err = a.muMPC.Update(a.muMPC.UpdateDataEnergyConsumed(v, &now, &state, nil, nil))
			case "energyProduced":
				err = a.muMPC.Update(a.muMPC.UpdateDataEnergyProduced(v, &now, &state, nil, nil))
			}
		}
		if err == nil {
			a.emit("usecase.measurement.set", c.ID, map[string]any{
				"usecase": a.cfg.Monitoring,
				"metric":  metric,
				"value":   v,
				"state":   state,
				"source":  "simulated-input",
			})
		}
	}
	a.result(c, err, nil)
}

func arenaMonitoringSelection(selection string) bool {
	return selection == "" || selection == "none" || selection == "mgcp" || selection == "mpc"
}
