
static EebusError ArenaHemsLimitLocked(HemsObject* object, const char* ski, const char* uc,
                                       double value, int duration, bool active, void* ctx) {
  Hems* self = HEMS(object);
  DeviceLocalObject* local = EEBUS_SERVICE_GET_LOCAL_DEVICE(self->service);
  DeviceRemoteObject* remote = DEVICE_LOCAL_GET_REMOTE_DEVICE_WITH_SKI(local, ski);
  if (!remote)
    return kEebusErrorInputArgument;
  const char* address = DEVICE_GET_ADDRESS(DEVICE_OBJECT(remote));
  EntityAddressList* list = strcmp(uc, "lpp") == 0 ? &self->eg_lpp_remotes : &self->eg_lpc_remotes;
  for (size_t i = 0; i < EntityAddressListGetSize(list); i++) {
    const EntityAddressType* entity = EntityAddressListGet(list, i);
    if (entity->device && strcmp(entity->device, address) == 0) {
      LoadLimit limit = {0};
      char scaled[80];
      snprintf(scaled, sizeof(scaled), "%.6f", value);
      if (ScaledValueParse(scaled, &limit.value) != kEebusErrorOk)
        return kEebusErrorInputArgument;
      limit.duration.seconds = duration;
      limit.is_active = active;
      return EgLpSetActivePowerLimit(strcmp(uc, "lpp") == 0 ? self->eg_lpp : self->eg_lpc, entity,
                                     &limit, ArenaWriteResult, ctx);
    }
  }
  return kEebusErrorInputArgument;
}

void ArenaHemsHeartbeat(HemsObject* object, bool enabled, unsigned period) {
  Hems* self = HEMS(object);
  DeviceLocalObject* local = EEBUS_SERVICE_GET_LOCAL_DEVICE(self->service);
  DEVICE_LOCAL_LOCK(local);
  const Vector* entities = DEVICE_LOCAL_GET_ENTITIES(local);
  for (size_t i = 0; i < VectorGetSize(entities); i++) {
    EntityLocalObject* entity = (EntityLocalObject*)VectorGetElement(entities, i);
    HeartbeatManagerObject* manager = ENTITY_LOCAL_GET_HEARTBEAT_MANAGER(entity);
    if (manager) {
      if (period)
        ArenaHeartbeatInterval(manager, period);
      if (enabled)
        HEARTBEAT_MANAGER_START(manager);
      else
        HEARTBEAT_MANAGER_STOP(manager);
    }
  }
  DEVICE_LOCAL_UNLOCK(local);
}

void ArenaHemsPartner(void* object, const char* uc, const EntityAddressType* entity, bool ready) {
  Hems* self = HEMS(object);
  DeviceLocalObject* local = EEBUS_SERVICE_GET_LOCAL_DEVICE(self->service);
  DeviceRemoteObject* remote = DEVICE_LOCAL_GET_REMOTE_DEVICE_WITH_ADDRESS(local, entity->device);
  if (remote)
    ArenaPartner(uc, DEVICE_REMOTE_GET_SKI(remote), ready);
}

EebusError ArenaHemsLimit(HemsObject* object, const char* ski, const char* uc, double value,
                          int duration, bool active, void* ctx) {
  DeviceLocalObject* local = EEBUS_SERVICE_GET_LOCAL_DEVICE(HEMS(object)->service);
  DEVICE_LOCAL_LOCK(local);
  EebusError err = ArenaHemsLimitLocked(object, ski, uc, value, duration, active, ctx);
  DEVICE_LOCAL_UNLOCK(local);
  return err;
}
