#ifndef ARENA_HOOKS_H
#define ARENA_HOOKS_H

#include <cjson/cJSON.h>
#include <stdbool.h>
#include "src/spine/api/entity_local_interface.h"
#include "src/spine/api/device_local_interface.h"
#include "src/spine/api/feature_local_interface.h"
#include "src/spine/api/heartbeat_manager_interface.h"
#include "src/use_case/model/scaled_value.h"
#include "src/common/eebus_date_time/eebus_duration.h"
#include "src/spine/api/message.h"

cJSON* ArenaConfig(void);
bool ArenaTrusted(const char* ski);
const char* ArenaNodeId(void);
unsigned ArenaTimeScale(void);

// Transfers ownership of data to the emitter, even if serialization fails.
void ArenaEmit(const char* type, const char* command_id, cJSON* data);
void ArenaPartner(const char* usecase, const char* ski, bool ready);
void ArenaHemsPartner(void* object, const char* usecase, const EntityAddressType* entity,
                      bool ready);
void ArenaPeer(const char* ski, const char* state);
void ArenaLimit(const char* usecase, const ScaledValue* value, const DurationType* duration,
                bool active);
void ArenaWriteResult(const ResultMessage* result, const FeatureAddressType* remote,
                      EebusError error, void* context);
void ArenaHeartbeatInterval(HeartbeatManagerObject* manager, unsigned period);

const char* ArenaMonitoringSelection(void);
EebusError ArenaMonitoringSetup(DeviceLocalObject* device);
cJSON* ArenaMonitoringSnapshot(void);
EebusError ArenaMonitoringSet(cJSON* parameters);

#ifdef ARENA_PAIRING
#include "src/service/api/eebus_service_interface.h"
#include "src/ship/api/ship_pairing_interface.h"
#include "src/ship/api/tls_certificate_interface.h"

bool ArenaPairingSetup(EebusServiceObject* service, const TlsCertificateObject* certificate);
cJSON* ArenaPairingSnapshot(void);
void ArenaPairingEvaluated(ShipPairingObject* pairing, const ShipPairingEntry* entry,
                           ShipPairingResult result);
void ArenaPairingEnabled(bool enabled);
bool ArenaPairingCommand(const char* command, cJSON* parameters, const char** error);
void ArenaPairingPeer(const char* ski, bool connected);
void ArenaPairingPoll(void);
void ArenaPairingTrace(const char* kind, cJSON* data);
#endif

#endif
