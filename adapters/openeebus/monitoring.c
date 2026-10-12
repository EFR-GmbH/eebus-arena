#include "arena_hooks.h"
#include <math.h>
#include <stdio.h>
#include <string.h>
#include "src/common/entity_address_list.h"
#include "src/spine/api/device_local_interface.h"
#include "src/spine/api/device_remote_interface.h"
#include "src/spine/entity/entity_local.h"
#include "src/use_case/actor/gcp/mgcp/gcp_mgcp.h"
#include "src/use_case/actor/mu/mpc/mu_mpc.h"
#include "src/use_case/actor/ma/mgcp/ma_mgcp.h"
#include "src/use_case/actor/ma/mpc/ma_mpc.h"

static DeviceLocalObject* device;
static GcpMgcpUseCaseObject* gcp;
static MuMpcUseCaseObject* mu;
static MaMgcpUseCaseObject* ma_gcp;
static MaMpcUseCaseObject* ma_mu;
static EntityAddressList gcp_remotes, mu_remotes;

const char* ArenaMonitoringSelection(void) {
  const cJSON* value = cJSON_GetObjectItem(ArenaConfig(), "monitoring");
  return cJSON_IsString(value) ? value->valuestring : "";
}

static const char* remote_ski(const EntityAddressType* address) {
  if (!address || !address->device)
    return NULL;
  DeviceRemoteObject* remote = DEVICE_LOCAL_GET_REMOTE_DEVICE_WITH_ADDRESS(device, address->device);
  return remote ? DEVICE_REMOTE_GET_SKI(remote) : NULL;
}

static void remote_event(const char* uc, EntityAddressList* list, const EntityAddressType* address,
                         bool connected) {
  if (connected) {
    EebusError err = EntityAddressListGetSize(list) < 256 ? EntityAddressListAdd(list, address)
                                                          : kEebusErrorNotSupported;
    if (err != kEebusErrorOk) {
      cJSON* data = cJSON_CreateObject();
      cJSON_AddStringToObject(data, "usecase", uc);
      cJSON_AddNumberToObject(data, "nativeError", err);
      ArenaEmit("usecase.monitoring.error", NULL, data);
      return;
    }
  }
  if (!connected)
    EntityAddressListRemove(list, address);
  const char* ski = remote_ski(address);
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "usecase", uc);
  if (ski)
    cJSON_AddStringToObject(data, "ski", ski);
  cJSON_AddBoolToObject(data, "ready", connected);
  ArenaEmit("usecase.partner", NULL, data);
}

static void gcp_connect(MaMgcpListenerObject* self, const EntityAddressType* address) {
  (void)self;
  remote_event("mgcp", &gcp_remotes, address, true);
}

static void gcp_disconnect(MaMgcpListenerObject* self, const EntityAddressType* address) {
  (void)self;
  remote_event("mgcp", &gcp_remotes, address, false);
}

static void mu_connect(MaMpcListenerObject* self, const EntityAddressType* address) {
  (void)self;
  remote_event("mpc", &mu_remotes, address, true);
}

static void mu_disconnect(MaMpcListenerObject* self, const EntityAddressType* address) {
  (void)self;
  remote_event("mpc", &mu_remotes, address, false);
}

static void measurement_event(const char* uc, int metric, const ScaledValue* value,
                              const EntityAddressType* address) {
  const char* ski = remote_ski(address);
  double number;
  if (!ski || ScaledValueToDouble(value, &number) != kEebusErrorOk)
    return;
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "usecase", uc);
  cJSON_AddStringToObject(data, "ski", ski);
  cJSON_AddNumberToObject(data, "nativeMetric", metric);
  cJSON_AddNumberToObject(data, "value", number);
  cJSON_AddStringToObject(data, "source", "native");
  ArenaEmit("usecase.measurement", NULL, data);
}

static void gcp_measure(MaMgcpListenerObject* self, GcpMeasurementNameId metric,
                        const ScaledValue* value, const EntityAddressType* address) {
  (void)self;
  measurement_event("mgcp", metric, value, address);
}

static void mu_measure(MaMpcListenerObject* self, MuMpcMeasurementNameId metric,
                       const ScaledValue* value, const EntityAddressType* address) {
  (void)self;
  measurement_event("mpc", metric, value, address);
}

static void gcp_factor(MaMgcpListenerObject* self, const ScaledValue* value,
                       const EntityAddressType* address) {
  (void)self;
  (void)value;
  (void)address;
}

static void gcp_destruct(MaMgcpListenerObject* self) {
  (void)self;
}

static void mu_destruct(MaMpcListenerObject* self) {
  (void)self;
}

static const MaMgcpListenerInterface gcp_listener_api = {gcp_destruct, gcp_connect, gcp_disconnect,
                                                         gcp_measure, gcp_factor};
static const MaMpcListenerInterface mu_listener_api = {mu_destruct, mu_connect, mu_disconnect,
                                                       mu_measure};
static MaMgcpListenerObject gcp_listener = {&gcp_listener_api};
static MaMpcListenerObject mu_listener = {&mu_listener_api};

static EntityLocalObject* new_entity(EntityTypeType type) {
  uint32_t id = (uint32_t)VectorGetSize(DEVICE_LOCAL_GET_ENTITIES(device));
  return EntityLocalCreate(device, type, &id, 1, 120);
}

EebusError ArenaMonitoringSetup(DeviceLocalObject* local) {
  device = local;
  EntityAddressListInit(&gcp_remotes);
  EntityAddressListInit(&mu_remotes);
  EntityLocalObject* consumer = new_entity(kEntityTypeTypeCEM);
  if (!consumer)
    return kEebusErrorMemoryAllocate;
  ma_gcp = MaMgcpUseCaseCreate(consumer, &gcp_listener);
  ma_mu = MaMpcUseCaseCreate(consumer, &mu_listener);
  if (!ma_gcp || !ma_mu) {
    EntityLocalDelete(consumer);
    return kEebusErrorInit;
  }
  DEVICE_LOCAL_ADD_ENTITY(device, consumer);
  const char* selection = ArenaMonitoringSelection();
  if (strcmp(selection, "mgcp") == 0) {
    const cJSON* role = cJSON_GetObjectItem(ArenaConfig(), "role");
    if (!cJSON_IsString(role) || strcmp(role->valuestring, "EG"))
      return kEebusErrorInputArgument;
    EntityLocalObject* provider = new_entity(kEntityTypeTypeGridConnectionPointOfPremises);
    static const GcpMgcpMeasurementConfig source = {.value_source =
                                                        kMeasurementValueSourceTypeCalculatedValue};
    static const GcpMgcpMonitorEnergyConfig energy = {.energy_feed_in_cfg = &source,
                                                      .energy_consumed_cfg = &source};
    static const GcpMgcpConfig cfg = {
        .power_cfg = {.phases = kElectricalConnectionPhaseNameTypeAbc,
                      .power_total_cfg = {.value_source =
                                              kMeasurementValueSourceTypeCalculatedValue}},
        .energy_cfg = &energy};
    if (!provider)
      return kEebusErrorMemoryAllocate;
    gcp = GcpMgcpUseCaseCreate(provider, 0, &cfg);
    if (!gcp) {
      EntityLocalDelete(provider);
      return kEebusErrorInit;
    }
    DEVICE_LOCAL_ADD_ENTITY(device, provider);
  } else if (strcmp(selection, "mpc") == 0) {
    EntityLocalObject* provider = new_entity(kEntityTypeTypeSubMeterElectricity);
    static const MuMpcMeasurementConfig source = {.value_source =
                                                      kMeasurementValueSourceTypeCalculatedValue};
    static const MuMpcMonitorEnergyConfig energy = {.energy_production_cfg = &source,
                                                    .energy_consumption_cfg = &source};
    static const MuMpcConfig cfg = {
        .power_cfg = {.power_total_cfg = {.value_source =
                                              kMeasurementValueSourceTypeCalculatedValue}},
        .energy_cfg = &energy};
    if (!provider)
      return kEebusErrorMemoryAllocate;
    mu = MuMpcUseCaseCreate(provider, 0, &cfg, NULL);
    if (!mu) {
      EntityLocalDelete(provider);
      return kEebusErrorInit;
    }
    DEVICE_LOCAL_ADD_ENTITY(device, provider);
  } else if (*selection && strcmp(selection, "none"))
    return kEebusErrorInputArgument;
  return kEebusErrorOk;
}

static cJSON* readings(bool grid, const EntityAddressType* remote) {
  cJSON* values = cJSON_CreateObject();
  const char* names[] = {"power", "energyConsumed", "energyProduced"};
  int ids[] = {kGcpPowerTotal, kGcpEnergyConsumed, grid ? kGcpEnergyFeedIn : kMpcEnergyProduced};
  for (int i = 0; i < 3; i++) {
    ScaledValue value = {0};
    EebusError err;
    if (remote)
      err = grid ? MaMgcpGetMeasurementData(ma_gcp, ids[i], remote, &value)
                 : MaMpcGetMeasurementData(ma_mu, ids[i], remote, &value);
    else
      err = grid ? GcpMgcpGetMeasurementData(gcp, ids[i], &value)
                 : MuMpcGetMeasurementData(mu, ids[i], &value);
    double number = 0;
    if (err == kEebusErrorOk)
      err = ScaledValueToDouble(&value, &number);
    cJSON* item = cJSON_CreateObject();
    cJSON_AddBoolToObject(item, "available", err == kEebusErrorOk);
    cJSON_AddStringToObject(item, "unit", i ? "Wh" : "W");
    cJSON_AddStringToObject(item, "source", "native");
    if (err == kEebusErrorOk)
      cJSON_AddNumberToObject(item, "value", number);
    else
      cJSON_AddNumberToObject(item, "nativeError", err);
    cJSON_AddItemToObject(values, names[i], item);
  }
  return values;
}

cJSON* ArenaMonitoringSnapshot(void) {
  cJSON* result = cJSON_CreateObject();
  cJSON_AddStringToObject(result, "offering", ArenaMonitoringSelection());
  if (!device)
    return result;
  DEVICE_LOCAL_LOCK(device);
  cJSON_AddItemToObject(result, "local",
                        gcp  ? readings(true, NULL)
                        : mu ? readings(false, NULL)
                             : cJSON_CreateObject());
  cJSON* remote = cJSON_CreateObject();
  cJSON_AddItemToObject(result, "remote", remote);
  for (int grid = 0; grid < 2; grid++) {
    EntityAddressList* list = grid ? &gcp_remotes : &mu_remotes;
    for (size_t i = 0; i < EntityAddressListGetSize(list); i++) {
      const EntityAddressType* address = EntityAddressListGet(list, i);
      const char* ski = remote_ski(address);
      if (!ski)
        continue;
      cJSON* peer = cJSON_GetObjectItem(remote, ski);
      if (!peer) {
        peer = cJSON_CreateObject();
        cJSON_AddItemToObject(remote, ski, peer);
      }
      cJSON_AddItemToObject(peer, grid ? "mgcp" : "mpc", readings(grid, address));
    }
  }
  DEVICE_LOCAL_UNLOCK(device);
  return result;
}

EebusError ArenaMonitoringSet(cJSON* params) {
  const cJSON *metric = cJSON_GetObjectItem(params, "metric"),
              *number = cJSON_GetObjectItem(params, "value"),
              *requested = cJSON_GetObjectItem(params, "state");
  if (!device || (!gcp && !mu) || !cJSON_IsString(metric) || !cJSON_IsNumber(number) ||
      !isfinite(number->valuedouble) || fabs(number->valuedouble) > 1e12)
    return kEebusErrorInputArgument;
  MeasurementValueStateType state = kMeasurementValueStateTypeNormal;
  if (requested) {
    if (!cJSON_IsString(requested))
      return kEebusErrorInputArgument;
    if (!strcmp(requested->valuestring, "error"))
      state = kMeasurementValueStateTypeError;
    else if (!strcmp(requested->valuestring, "outOfRange"))
      state = kMeasurementValueStateTypeOutofrange;
    else if (strcmp(requested->valuestring, "normal"))
      return kEebusErrorInputArgument;
  }
  int id;
  if (!strcmp(metric->valuestring, "power"))
    id = kGcpPowerTotal;
  else if (!strcmp(metric->valuestring, "energyConsumed") && number->valuedouble >= 0)
    id = kGcpEnergyConsumed;
  else if (!strcmp(metric->valuestring, "energyProduced") && number->valuedouble <= 0)
    id = gcp ? kGcpEnergyFeedIn : kMpcEnergyProduced;
  else
    return kEebusErrorInputArgument;
  char encoded[80];
  snprintf(encoded, sizeof(encoded), "%.6f", number->valuedouble);
  ScaledValue value = {0};
  EebusError err = ScaledValueParse(encoded, &value);
  if (err != kEebusErrorOk)
    return err;
  DEVICE_LOCAL_LOCK(device);
  err = gcp ? GcpMgcpSetMeasurementDataCache(gcp, id, &value, NULL, &state)
            : MuMpcSetMeasurementDataCache(mu, id, &value, NULL, &state);
  if (err == kEebusErrorOk)
    err = gcp ? GcpMgcpUpdate(gcp) : MuMpcUpdate(mu);
  DEVICE_LOCAL_UNLOCK(device);
  if (err == kEebusErrorOk) {
    cJSON* data = cJSON_Duplicate(params, true);
    cJSON_AddStringToObject(data, "usecase", ArenaMonitoringSelection());
    cJSON_AddStringToObject(data, "source", "simulated-input");
    ArenaEmit("usecase.measurement.set", NULL, data);
  }
  return err;
}
