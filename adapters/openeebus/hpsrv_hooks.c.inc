
void ArenaHpsrvHeartbeat(HpsrvObject* object, bool enabled, unsigned period) {
  Hpsrv* self = HPSRV(object);
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
