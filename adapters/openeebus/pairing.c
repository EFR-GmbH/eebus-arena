#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <unistd.h>
#include "arena_hooks.h"
#include "src/common/string_util.h"
#include "src/service/service/eebus_service.h"
#include "src/ship/api/ship_node_interface.h"
#include "src/ship/api/ship_pairing_interface.h"
#include "src/ship/ship_pairing/ship_pairing_request.h"

static pthread_mutex_t pairing_mutex = PTHREAD_MUTEX_INITIALIZER;
static EebusServiceObject* pairing_service;
static const TlsCertificateObject* pairing_cert;
static ShipPairingRequest* request;
static cJSON* pairing_state;
static cJSON* announcements;
static bool configured;
static char* target_id;
static char* target_ski;
static const char* state_file = "/state/pairing.json";
extern cJSON* ArenaConfig(void);
extern EebusError ArenaRestoreTrust(ShipNodeObject*, const char*, const char*, bool);

static const char* text(const cJSON* object, const char* key) {
  const cJSON* value = cJSON_GetObjectItemCaseSensitive(object, key);
  return cJSON_IsString(value) ? value->valuestring : "";
}

static void persistence_error(const char* reason) {
  cJSON* data = cJSON_CreateObject();
  cJSON_AddBoolToObject(data, "ok", false);
  cJSON_AddStringToObject(data, "error", reason);
  ArenaEmit("pairing.persistence", NULL, data);
  cJSON_DeleteItemFromObject(pairing_state, "persistenceError");
  cJSON_AddStringToObject(pairing_state, "persistenceError", reason);
}

// Sync both the file and its directory: a successful rename alone does not
// guarantee that trust and replay history survive a power failure together.
static bool save_locked(void) {
  char* encoded = cJSON_PrintUnformatted(pairing_state);
  if (!encoded)
    return false;
  char path[] = "/state/.pairing-XXXXXX";
  int fd = mkstemp(path);
  bool ok = fd >= 0;
  size_t size = strlen(encoded), offset = 0;
  while (ok && offset < size) {
    ssize_t n = write(fd, encoded + offset, size - offset);
    if (n < 0 && errno == EINTR)
      continue;
    if (n <= 0) {
      ok = false;
      break;
    }
    offset += (size_t)n;
  }
  if (ok && fsync(fd))
    ok = false;
  if (fd >= 0 && close(fd))
    ok = false;
  if (ok && rename(path, state_file))
    ok = false;
  int dir = open("/state", O_RDONLY);
  if (ok && (dir < 0 || fsync(dir)))
    ok = false;
  if (dir >= 0)
    close(dir);
  unlink(path);
  free(encoded);
  if (!ok)
    persistence_error("Could not durably save pairing state");
  return ok;
}

// The native API exposes an opaque struct. Its byte representation is only
// portable within the pinned upstream ABI; review persistence when changing it.
static void save_ring_locked(ShipPairingObject* pairing) {
  ShipPairingRingBuffer ring = {0};
  SHIP_PAIRING_GET_RING_BUFFER(pairing, &ring);
  char hex[sizeof(ring) * 2 + 1];
  const unsigned char* bytes = (const unsigned char*)&ring;
  for (size_t i = 0; i < sizeof(ring); i++)
    snprintf(hex + i * 2, 3, "%02x", bytes[i]);
  cJSON_DeleteItemFromObject(pairing_state, "ringBuffer");
  cJSON_AddStringToObject(pairing_state, "ringBuffer", hex);
}

static void trust_locked(const char* ship_id, const char* fingerprint, const char* ski,
                         bool listener) {
  cJSON* trusted = cJSON_GetObjectItem(pairing_state, "trusted");
  if (listener) {
    cJSON* old;
    cJSON_ArrayForEach(old, trusted) {
      if (strcmp(old->string, fingerprint)) {
        cJSON* event = cJSON_CreateObject();
        cJSON_AddItemToObject(event, "identity", cJSON_Duplicate(old, true));
        cJSON_AddBoolToObject(event, "trusted", false);
        cJSON_AddStringToObject(event, "reason", "Replaced by native addCu acceptance");
        ArenaEmit("pairing.trust", NULL, event);
      }
    }
    cJSON_DeleteItemFromObject(pairing_state, "trusted");
    trusted = cJSON_AddObjectToObject(pairing_state, "trusted");
  }
  cJSON_DeleteItemFromObject(trusted, fingerprint);
  cJSON* identity = cJSON_AddObjectToObject(trusted, fingerprint);
  cJSON_AddStringToObject(identity, "shipID", ship_id);
  cJSON_AddStringToObject(identity, "fingerprint", fingerprint);
  cJSON_AddStringToObject(identity, "ski", ski ? ski : "");
  cJSON_AddNumberToObject(identity, "pairingType", listener ? 1 : 0);
  cJSON* event = cJSON_CreateObject();
  cJSON_AddItemToObject(event, "identity", cJSON_Duplicate(identity, true));
  cJSON_AddBoolToObject(event, "trusted", true);
  ArenaEmit("pairing.trust", NULL, event);
}

void ArenaPairingEnabled(bool enabled) {
  if (!configured)
    return;
  pthread_mutex_lock(&pairing_mutex);
  cJSON* listener = cJSON_GetObjectItem(pairing_state, "listener");
  cJSON_DeleteItemFromObject(listener, "active");
  cJSON_AddBoolToObject(listener, "active", enabled);
  pthread_mutex_unlock(&pairing_mutex);
  cJSON* data = cJSON_CreateObject();
  cJSON_AddBoolToObject(data, "active", enabled);
  ArenaEmit("pairing.listener", NULL, data);
}

void ArenaPairingEvaluated(ShipPairingObject* object, const ShipPairingEntry* entry,
                           ShipPairingResult result) {
  if (!configured)
    return;
  const char* reasons[] = {"accepted",       "invalid-record", "not-for-this-node", "unsupported",
                           "not-processing", "already-seen",   "digest-mismatch",   "error"};
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "shipId", entry->trust_id ? entry->trust_id : "");
  cJSON_AddStringToObject(data, "digest", entry->digest ? entry->digest : "");
  cJSON_AddStringToObject(data, "reason",
                          result <= kShipPairingResultError ? reasons[result] : "unknown");
  cJSON_AddNumberToObject(data, "nativeResult", result);
  cJSON_AddStringToObject(data, "stage", "outcome");
  cJSON_AddBoolToObject(data, "txtversFirst", entry->txtvers_is_first);
  cJSON_AddStringToObject(data, "txtvers", entry->txtvers ? entry->txtvers : "");
  cJSON_AddStringToObject(data, "curve", entry->trust_curve ? entry->trust_curve : "");
  cJSON_AddBoolToObject(data, "ok", result == kShipPairingResultAccepted);
  ArenaPairingTrace("pairing.validation", data);
  if (result != kShipPairingResultAccepted)
    return;
  pthread_mutex_lock(&pairing_mutex);
  trust_locked(entry->trust_id, entry->trust_par, "", true);
  save_ring_locked(object);
  save_locked();
  pthread_mutex_unlock(&pairing_mutex);
}

bool ArenaPairingSetup(EebusServiceObject* service, const TlsCertificateObject* cert) {
  cJSON* cfg = ArenaConfig();
  const char* role = text(cfg, "pairingRole");
  if (!*role)
    return true;
  pairing_service = service;
  pairing_cert = cert;
  pairing_state = cJSON_CreateObject();
  cJSON_AddNumberToObject(pairing_state, "version", 1);
  cJSON_AddStringToObject(pairing_state, "role", role);
  cJSON_AddObjectToObject(pairing_state, "trusted");
  cJSON_AddObjectToObject(pairing_state, "listener");
  announcements = cJSON_CreateArray();
  FILE* file = fopen(state_file, "r");
  if (file) {
    char buffer[65536];
    size_t size = fread(buffer, 1, sizeof(buffer) - 1, file);
    bool complete = !ferror(file) && feof(file);
    fclose(file);
    buffer[size] = 0;
    cJSON* loaded = cJSON_Parse(buffer);
    if (!complete || !loaded || !cJSON_IsObject(cJSON_GetObjectItem(loaded, "trusted")) ||
        !cJSON_IsObject(cJSON_GetObjectItem(loaded, "listener")) ||
        strcmp(text(loaded, "role"), role) ||
        !cJSON_IsNumber(cJSON_GetObjectItem(loaded, "version")) ||
        cJSON_GetObjectItem(loaded, "version")->valueint != 1) {
      cJSON_Delete(loaded);
      return false;
    }
    cJSON_Delete(pairing_state);
    pairing_state = loaded;
  } else if (errno != ENOENT)
    return false;
  ShipNodeObject* node = EebusServiceGetShipNode(service);
  ShipPairingObject* pairing = SHIP_NODE_GET_SHIP_PAIRING(node);
  if (!pairing)
    return false;
  const char* ring_hex = text(pairing_state, "ringBuffer");
  if (*ring_hex) {
    ShipPairingRingBuffer ring = {0};
    if (strlen(ring_hex) != sizeof(ring) * 2 ||
        !StringHexToBytes(ring_hex, (uint8_t*)&ring, sizeof(ring)) ||
        ring.next > SHIP_PAIRING_RING_BUFFER_SIZE)
      return false;
    for (size_t i = 0; i < SHIP_PAIRING_RING_BUFFER_SIZE; i++) {
      if (!memchr(ring.entries[i].alg, 0, sizeof(ring.entries[i].alg)) ||
          !memchr(ring.entries[i].digest, 0, sizeof(ring.entries[i].digest)))
        return false;
    }
    SHIP_PAIRING_SET_RING_BUFFER(pairing, &ring);
  }
  cJSON* identity;
  cJSON_ArrayForEach(identity, cJSON_GetObjectItem(pairing_state, "trusted")) {
    const char *fingerprint = text(identity, "fingerprint"), *id = text(identity, "shipID");
    if (strlen(fingerprint) != 64 || !*id ||
        ArenaRestoreTrust(node, id, fingerprint, strcmp(role, "listener") == 0) != kEebusErrorOk)
      return false;
  }
  configured = true;
  if (strcmp(role, "listener") == 0) {
    uint8_t secret[16];
    const char* value = text(cJSON_GetObjectItem(cfg, "pairing"), "secret");
    if (strlen(value) != 32 || !StringHexToBytes(value, secret, sizeof(secret)))
      return false;
    SHIP_PAIRING_SET_SECRET(pairing, secret, sizeof(secret));
    memset(secret, 0, sizeof(secret));
  }
  return true;
}

cJSON* ArenaPairingSnapshot(void) {
  if (!configured)
    return NULL;
  pthread_mutex_lock(&pairing_mutex);
  cJSON* result = cJSON_Duplicate(pairing_state, true);
  cJSON_DeleteItemFromObject(result, "ringBuffer");
  cJSON_AddItemToObject(result, "announcements", cJSON_Duplicate(announcements, true));
  pthread_mutex_unlock(&pairing_mutex);
  return result;
}

bool ArenaPairingCommand(const char* name, cJSON* params, const char** error) {
  *error = NULL;
  if (!configured) {
    *error = "Pairing not configured";
    return false;
  }
  if (!strcmp(name, "pairing.status"))
    return true;
  ShipNodeObject* node = EebusServiceGetShipNode(pairing_service);
  pthread_mutex_lock(&pairing_mutex);
  if (!strcmp(name, "pairing.stop")) {
    const char* id = text(params, "shipId");
    if (target_id && *id && strcmp(target_id, id)) {
      *error = "No active announcement for this target";
      pthread_mutex_unlock(&pairing_mutex);
      return false;
    }
    if (request)
      ShipPairingRequestStop(request);
    if (SHIP_NODE_ANNOUNCE_SHIP_PAIRING_REQUEST(node, NULL) != kEebusErrorOk)
      *error = "Native withdrawal failed";
    else {
      cJSON_Delete(announcements);
      announcements = cJSON_CreateArray();
    }
  } else if (!strcmp(name, "pairing.announce")) {
    cJSON* target = cJSON_GetObjectItem(params, "target");
    const char *id = text(target, "shipId"), *fingerprint = text(target, "fingerprint"),
               *secret_hex = text(target, "secret");
    uint8_t secret[16];
    if (!*id || strlen(fingerprint) != 64 || strlen(secret_hex) != 32 ||
        !StringHexToBytes(secret_hex, secret, sizeof(secret))) {
      *error = "Invalid pairing target";
      pthread_mutex_unlock(&pairing_mutex);
      return false;
    }
    if (target_id && strcmp(target_id, id) && request && ShipPairingRequestIsAnnouncing(request)) {
      *error = "Native adapter supports one active target; stop it first";
      pthread_mutex_unlock(&pairing_mutex);
      return false;
    }
    ShipPairingRequestConfig cfg = {.instance_name = ArenaNodeId(),
                                    .for_id = id,
                                    .for_par = fingerprint,
                                    .secret = secret,
                                    .secret_size = 16,
                                    .trust_id = ArenaNodeId(),
                                    .tls_certificate = pairing_cert};
    ShipPairingRequest* next = ShipPairingRequestCreate(&cfg);
    memset(secret, 0, sizeof(secret));
    if (!next ||
        SHIP_NODE_REGISTER_REMOTE_FINGERPRINT_WITH_SHIP_ID(node, id, fingerprint) != kEebusErrorOk)
      *error = "Native target registration failed";
    else if (SHIP_NODE_ANNOUNCE_SHIP_PAIRING_REQUEST(node, ShipPairingRequestGetEntry(next)) !=
             kEebusErrorOk)
      *error = "Native announcement failed";
    if (*error) {
      ShipPairingRequestDelete(next);
    } else {
      ShipPairingRequestDelete(request);
      request = next;
      free(target_id);
      target_id = strdup(id);
      free(target_ski);
      target_ski = strdup(text(target, "ski"));
      cJSON_Delete(announcements);
      announcements = cJSON_CreateArray();
      cJSON_AddItemToArray(announcements, cJSON_CreateString(id));
      trust_locked(id, fingerprint, target_ski, false);
      if (!save_locked())
        *error = "Pairing state persistence failed";
    }
  } else
    *error = "Unknown pairing command";
  pthread_mutex_unlock(&pairing_mutex);
  return *error == NULL;
}

void ArenaPairingPeer(const char* ski, bool connected) {
  if (!configured)
    return;
  pthread_mutex_lock(&pairing_mutex);
  if (request && target_ski && !strcmp(target_ski, ski)) {
    if (connected)
      ShipPairingRequestNotifyConnected(request);
    else
      ShipPairingRequestNotifyDisconnected(request);
  }
  pthread_mutex_unlock(&pairing_mutex);
}

void ArenaPairingPoll(void) {
  if (!configured)
    return;
  pthread_mutex_lock(&pairing_mutex);
  if (request && !ShipPairingRequestIsAnnouncing(request) &&
      cJSON_GetArraySize(announcements) > 0) {
    SHIP_NODE_ANNOUNCE_SHIP_PAIRING_REQUEST(EebusServiceGetShipNode(pairing_service), NULL);
    cJSON_Delete(announcements);
    announcements = cJSON_CreateArray();
  }
  pthread_mutex_unlock(&pairing_mutex);
}
