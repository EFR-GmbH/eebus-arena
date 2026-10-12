#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include "arena_hooks.h"
#include "examples/hems/hems.h"
#include "examples/heat_pump/hpsrv.h"
#include "src/ship/tls_certificate/tls_certificate.h"

static FILE* protocol;
static cJSON* config;
static cJSON* snapshot;
static pthread_mutex_t mutex = PTHREAD_MUTEX_INITIALIZER;
static unsigned long sequence;
static char incarnation[80];
static volatile sig_atomic_t stopping;
static HemsObject* hems;
static HpsrvObject* hpsrv;
static bool heartbeat_enabled = true;
static pid_t dbus_pid, avahi_pid;
extern EebusError ArenaHemsLimit(HemsObject*, const char*, const char*, double, int, bool, void*);
extern void ArenaHemsHeartbeat(HemsObject*, bool, unsigned);
extern void ArenaHpsrvHeartbeat(HpsrvObject*, bool, unsigned);

cJSON* ArenaConfig(void) {
  return config;
}

const char* ArenaNodeId(void) {
  return cJSON_GetObjectItem(config, "nodeId")->valuestring;
}

unsigned ArenaTimeScale(void) {
  return (unsigned)cJSON_GetObjectItem(config, "timeScale")->valueint;
}

bool ArenaTrusted(const char* ski) {
  cJSON* item;
  cJSON_ArrayForEach(item, cJSON_GetObjectItem(config, "trustedSkis")) {
    if (cJSON_IsString(item) && strcmp(item->valuestring, ski) == 0)
      return true;
  }
  return false;
}
// The emitter owns data, including when serialization fails. Callers must not
// reuse it after this call. Serialize under one lock to preserve event ordering.
void ArenaEmit(const char* type, const char* command_id, cJSON* data) {
  pthread_mutex_lock(&mutex);
  cJSON* event = cJSON_CreateObject();
  cJSON_AddStringToObject(event, "type", type);
  cJSON_AddStringToObject(event, "nodeId", ArenaNodeId());
  cJSON_AddStringToObject(event, "incarnation", incarnation);
  cJSON_AddNumberToObject(event, "sequence", ++sequence);
  if (command_id)
    cJSON_AddStringToObject(event, "commandId", command_id);
  char stamp[80];
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  struct tm tm;
  gmtime_r(&ts.tv_sec, &tm);
  strftime(stamp, sizeof(stamp), "%Y-%m-%dT%H:%M:%S", &tm);
  char full[100];
  snprintf(full, sizeof(full), "%s.%09ldZ", stamp, ts.tv_nsec);
  cJSON_AddStringToObject(event, "at", full);
  cJSON_AddItemToObject(event, "data", data ? data : cJSON_CreateObject());
  char* encoded = cJSON_PrintUnformatted(event);
  if (encoded) {
    fprintf(protocol, "%s\n", encoded);
    fflush(protocol);
    free(encoded);
  }
  cJSON_Delete(event);
  pthread_mutex_unlock(&mutex);
}

void ArenaPartner(const char* uc, const char* ski, bool ready) {
  pthread_mutex_lock(&mutex);
  cJSON *partners = cJSON_GetObjectItem(snapshot, "partners"),
        *group = cJSON_GetObjectItem(partners, uc);
  cJSON_DeleteItemFromObject(group, ski);
  cJSON_AddBoolToObject(group, ski, ready);
  pthread_mutex_unlock(&mutex);
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "usecase", uc);
  cJSON_AddStringToObject(data, "ski", ski);
  cJSON_AddBoolToObject(data, "ready", ready);
  ArenaEmit("usecase.partner", NULL, data);
}

void ArenaPeer(const char* ski, const char* state) {
#ifdef ARENA_PAIRING
  ArenaPairingPeer(ski, strcmp(state, "connected") == 0);
#endif
  pthread_mutex_lock(&mutex);
  cJSON* peers = cJSON_GetObjectItem(snapshot, "peers");
  cJSON_DeleteItemFromObject(peers, ski);
  cJSON_AddStringToObject(peers, ski, state);
  pthread_mutex_unlock(&mutex);
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "ski", ski);
  cJSON_AddStringToObject(data, "state", state);
  ArenaEmit("ship.state", NULL, data);
}

void ArenaLimit(const char* usecase, const ScaledValue* value, const DurationType* duration,
                bool active) {
  double watts = 0;
  ScaledValueToDouble(value, &watts);
  cJSON* data = cJSON_CreateObject();
  cJSON_AddStringToObject(data, "usecase", usecase);
  cJSON_AddNumberToObject(data, "value", watts);
  cJSON_AddBoolToObject(data, "active", active);
  cJSON_AddNumberToObject(data, "durationSeconds", EebusDurationToSeconds(duration));
  pthread_mutex_lock(&mutex);
  cJSON_DeleteItemFromObject(snapshot, usecase);
  cJSON_AddItemToObject(snapshot, usecase, cJSON_Duplicate(data, true));
  pthread_mutex_unlock(&mutex);
  ArenaEmit("usecase.limit", NULL, data);
}
typedef struct {
  char* id;
  struct timespec started;
} WriteRequest;
static void result(const char* id, bool ok, const char* error) {
  cJSON* data = cJSON_CreateObject();
  cJSON_AddBoolToObject(data, "ok", ok);
  if (error)
    cJSON_AddStringToObject(data, "error", error);
  ArenaEmit("command.result", id, data);
}

void ArenaWriteResult(const ResultMessage* msg, const FeatureAddressType* remote, EebusError err,
                      void* ctx) {
  (void)remote;
  WriteRequest* request = ctx;
  const char* id = request->id;
  struct timespec now;
  clock_gettime(CLOCK_MONOTONIC, &now);
  double elapsed = (now.tv_sec - request->started.tv_sec) * 1000.0 +
                   (now.tv_nsec - request->started.tv_nsec) / 1000000.0;
  bool ok = err == kEebusErrorOk && msg && msg->result_data && msg->result_data->error_number &&
            *msg->result_data->error_number == kErrorNumberTypeNoError;
  result(id, ok, ok ? NULL : "SPINE rejected the write or response timed out");
  cJSON* data = cJSON_CreateObject();
  cJSON_AddBoolToObject(data, "ok", ok);
  cJSON_AddNumberToObject(data, "elapsedMs", elapsed);
  if (msg)
    cJSON_AddNumberToObject(data, "messageCounter", msg->msg_cnt_ref);
  ArenaEmit("spine.response", id, data);
  free(request->id);
  free(request);
}

static void stop_signal(int signal_number) {
  (void)signal_number;
  stopping = 1;
}

static pid_t spawn(char* const args[]) {
  pid_t pid = fork();
  if (pid == 0) {
    execv(args[0], args);
    _exit(127);
  }
  return pid;
}

static bool configure_avahi(const char* mode) {
  FILE* input = fopen("/etc/avahi/avahi-daemon.conf", "r");
  if (!input)
    return false;
  FILE* output = fopen("/tmp/arena-avahi.conf", "w");
  if (!output) {
    fclose(input);
    return false;
  }
  char* line = NULL;
  size_t capacity = 0;
  bool server = false;
  unsigned changed = 0;
  while (getline(&line, &capacity, input) >= 0) {
    if (line[0] == '[')
      server = strncmp(line, "[server]", 8) == 0;
    if (server && strncmp(line, "use-ipv4=", 9) == 0) {
      fprintf(output, "use-ipv4=%s\n", strcmp(mode, "ipv6") ? "yes" : "no");
      changed |= 1;
    } else if (server && strncmp(line, "use-ipv6=", 9) == 0) {
      fprintf(output, "use-ipv6=%s\n", strcmp(mode, "ipv4") ? "yes" : "no");
      changed |= 2;
    } else if (server && (strncmp(line, "allow-interfaces=", 17) == 0 ||
                          strncmp(line, "#allow-interfaces=", 18) == 0)) {
      fputs("allow-interfaces=eth0\n", output);
      changed |= 4;
    } else
      fputs(line, output);
  }
  free(line);
  bool ok = changed == 7 && !ferror(input) && !ferror(output);
  if (fclose(input) != 0)
    ok = false;
  if (fclose(output) != 0)
    ok = false;
  return ok;
}

static void handle_heartbeat(const char* name, const char* id, cJSON* params) {
  bool enabled = heartbeat_enabled;
  unsigned period = 0;
  if (strcmp(name, "heartbeat.set") == 0) {
    cJSON* p = cJSON_GetObjectItem(params, "enabled");
    if (!cJSON_IsBool(p)) {
      result(id, false, "enabled must be a boolean");
      return;
    }
    enabled = cJSON_IsTrue(p);
    heartbeat_enabled = enabled;
  } else {
    cJSON* p = cJSON_GetObjectItem(params, "seconds");
    if (!cJSON_IsNumber(p) || p->valuedouble < 1 || p->valuedouble > 3600) {
      result(id, false, "seconds must be 1–3600");
      return;
    }
    period = (unsigned)p->valuedouble;
  }
  if (hems)
    ArenaHemsHeartbeat(hems, enabled, period);
  else
    ArenaHpsrvHeartbeat(hpsrv, enabled, period);
  result(id, true, NULL);
}

static void handle_limit(const char* id, cJSON* params) {
  cJSON *ski = cJSON_GetObjectItem(params, "targetSki"),
        *uc = cJSON_GetObjectItem(params, "usecase"), *value = cJSON_GetObjectItem(params, "value"),
        *active = cJSON_GetObjectItem(params, "active"),
        *duration = cJSON_GetObjectItem(params, "durationSeconds");
  if (!cJSON_IsString(ski) || !cJSON_IsString(uc) || !cJSON_IsNumber(value) ||
      !cJSON_IsBool(active) || !cJSON_IsNumber(duration) || duration->valuedouble < 1 ||
      duration->valuedouble > 86400 ||
      (strcmp(uc->valuestring, "lpc") && strcmp(uc->valuestring, "lpp"))) {
    result(id, false, "Invalid limit parameters");
    return;
  }
  WriteRequest* context = calloc(1, sizeof(*context));
  if (!context) {
    result(id, false, "Allocation failed");
    return;
  }
  context->id = strdup(id);
  if (!context->id) {
    free(context);
    result(id, false, "Allocation failed");
    return;
  }
  // The native write completes asynchronously. Its callback owns this request
  // after a successful submission; only a submission failure is freed here.
  clock_gettime(CLOCK_MONOTONIC, &context->started);
  EebusError err = ArenaHemsLimit(hems, ski->valuestring, uc->valuestring, value->valuedouble,
                                  duration->valueint, cJSON_IsTrue(active), context);
  if (err != kEebusErrorOk) {
    free(context->id);
    free(context);
    result(id, false, "Partner is not ready or write failed");
  }
}

static void handle(cJSON* cmd) {
  const cJSON* name = cJSON_GetObjectItem(cmd, "command");
  const cJSON* id = cJSON_GetObjectItem(cmd, "id");
  cJSON* params = cJSON_GetObjectItem(cmd, "params");
  if (!cJSON_IsString(name) || !cJSON_IsString(id)) {
    ArenaEmit("command.invalid", NULL, NULL);
    return;
  }
#ifdef ARENA_PAIRING
  if (strncmp(name->valuestring, "pairing.", 8) == 0) {
    const char* error = NULL;
    bool ok = ArenaPairingCommand(name->valuestring, params, &error);
    cJSON* data = cJSON_CreateObject();
    cJSON_AddBoolToObject(data, "ok", ok);
    if (error)
      cJSON_AddStringToObject(data, "error", error);
    cJSON* status = ArenaPairingSnapshot();
    if (status)
      cJSON_AddItemToObject(data, "pairing", status);
    ArenaEmit("command.result", id->valuestring, data);
    return;
  }
#endif
  if (strcmp(name->valuestring, "snapshot") == 0) {
    pthread_mutex_lock(&mutex);
    cJSON* copy = cJSON_Duplicate(snapshot, true);
    pthread_mutex_unlock(&mutex);
    ArenaEmit("snapshot", NULL, copy);
    result(id->valuestring, true, NULL);
  } else if (strcmp(name->valuestring, "heartbeat.set") == 0 ||
             strcmp(name->valuestring, "heartbeat.interval") == 0) {
    handle_heartbeat(name->valuestring, id->valuestring, params);
  } else if (strcmp(name->valuestring, "measurement.set") == 0) {
    EebusError err = ArenaMonitoringSet(params);
    result(id->valuestring, err == kEebusErrorOk,
           err == kEebusErrorOk ? NULL : "Invalid or unavailable native monitoring measurement");
  } else if (strcmp(name->valuestring, "measurement.read") == 0) {
    cJSON* data = cJSON_CreateObject();
    cJSON_AddBoolToObject(data, "ok", true);
    cJSON_AddItemToObject(data, "monitoring", ArenaMonitoringSnapshot());
    ArenaEmit("command.result", id->valuestring, data);
  } else if (strcmp(name->valuestring, "limit.set") == 0 && hems) {
    handle_limit(id->valuestring, params);
  } else
    result(id->valuestring, false, "Unsupported command");
}

static bool load_configuration(void) {
  FILE* file = fopen("/identity/config.json", "r");
  if (!file)
    return false;
  char cfg[65536];
  size_t length = fread(cfg, 1, sizeof(cfg) - 1, file);
  fclose(file);
  cfg[length] = '\0';
  config = cJSON_Parse(cfg);
  if (!config)
    return false;
  return true;
}

static void initialize_snapshot(void) {
  snapshot = cJSON_CreateObject();
  cJSON_AddItemToObject(snapshot, "peers", cJSON_CreateObject());
  cJSON_AddStringToObject(snapshot, "stateSource", "observed");
  cJSON* partners = cJSON_CreateObject();
  cJSON_AddItemToObject(partners, "lpc", cJSON_CreateObject());
  cJSON_AddItemToObject(partners, "lpp", cJSON_CreateObject());
  cJSON_AddItemToObject(snapshot, "partners", partners);
  struct timespec ts;
  clock_gettime(CLOCK_REALTIME, &ts);
  snprintf(incarnation, sizeof(incarnation), "%ld-%ld", ts.tv_sec, ts.tv_nsec);
}

static bool start_discovery_services(void) {
  // Avahi depends on D-Bus. Wait for its socket before launching discovery.
  char* dbus_args[] = {"/usr/bin/dbus-daemon", "--system", "--nofork", "--nopidfile", NULL};
  dbus_pid = spawn(dbus_args);
  if (dbus_pid < 0)
    return false;
  for (int i = 0; i < 50 && access("/run/dbus/system_bus_socket", F_OK) != 0; i++) {
    struct timespec delay = {0, 100000000};
    nanosleep(&delay, NULL);
  }
  const cJSON* mode_value = cJSON_GetObjectItem(config, "networkMode");
  const char* mode = cJSON_IsString(mode_value) ? mode_value->valuestring : "ipv4";
  if (strcmp(mode, "ipv4") && strcmp(mode, "ipv6") && strcmp(mode, "dual-stack"))
    return false;
  if (!configure_avahi(mode))
    return false;
  cJSON_AddStringToObject(snapshot, "networkMode", mode);
  char* avahi_args[] = {"/usr/sbin/avahi-daemon", "--no-drop-root", "--no-chroot",
                        "--file=/tmp/arena-avahi.conf", NULL};
  avahi_pid = spawn(avahi_args);
  if (avahi_pid < 0)
    return false;
  struct timespec delay = {1, 0};
  nanosleep(&delay, NULL);
  return true;
}

static bool start_native_stack(void) {
  TlsCertificateObject* cert =
      TlsCertificateLoadX509KeyPair(cJSON_GetObjectItem(config, "certificate")->valuestring,
                                    cJSON_GetObjectItem(config, "privateKey")->valuestring);
  if (!cert)
    return false;
  cJSON_AddStringToObject(snapshot, "localSki", TLS_CERTIFICATE_GET_SKI(cert));
  bool eg = strcmp(cJSON_GetObjectItem(config, "role")->valuestring, "EG") == 0;
  if (eg)
    hems = HemsOpen(cJSON_GetObjectItem(config, "port")->valueint, "auto", cert);
  else
    hpsrv = HpsrvOpen(cJSON_GetObjectItem(config, "port")->valueint, "auto", cert);
  if (!hems && !hpsrv)
    return false;
  cJSON* ski;
  cJSON_ArrayForEach(ski, cJSON_GetObjectItem(config, "trustedSkis")) {
    if (hems)
      HemsRegisterRemoteSki(hems, ski->valuestring);
    else
      HpsrvRegisterRemoteSki(hpsrv, ski->valuestring);
  }
  return true;
}

static void emit_capabilities(void) {
  cJSON *caps = cJSON_CreateObject(), *commands = cJSON_CreateArray();
  for (const char** p =
           (const char*[]){"snapshot", "heartbeat.set", "heartbeat.interval", "observe.limit",
                           "observe.ship", "observe.partners", "timing.accelerated",
                           "measurement.read", "observe.measurement", "monitoring.mgcp",
                           "monitoring.mpc", NULL};
       *p; p++)
    cJSON_AddItemToArray(commands, cJSON_CreateString(*p));
  if (hems)
    cJSON_AddItemToArray(commands, cJSON_CreateString("limit.set"));
  if (!strcmp(ArenaMonitoringSelection(), "mgcp") || !strcmp(ArenaMonitoringSelection(), "mpc"))
    cJSON_AddItemToArray(commands, cJSON_CreateString("measurement.set"));
#ifdef ARENA_PAIRING
  if (cJSON_IsString(cJSON_GetObjectItem(config, "pairingRole"))) {
    for (const char** p =
             (const char*[]){"pairing.status", "observe.pairing", "observe.pairing.trace", NULL};
         *p; p++)
      cJSON_AddItemToArray(commands, cJSON_CreateString(*p));
    if (hems) {
      cJSON_AddItemToArray(commands, cJSON_CreateString("pairing.announce"));
      cJSON_AddItemToArray(commands, cJSON_CreateString("pairing.stop"));
      cJSON_AddItemToArray(commands, cJSON_CreateString("pairing.single-target"));
    } else
      cJSON_AddItemToArray(commands, cJSON_CreateString("pairing.listen"));
  }
#endif
  cJSON_AddItemToObject(caps, "commands", commands);
  cJSON_AddStringToObject(caps, "gap",
                          "Upstream demo exposes protocol values but no full CS state machine");
  ArenaEmit("capabilities", NULL, caps);
}

static void publish_snapshot(void) {
#ifdef ARENA_PAIRING
  ArenaPairingPoll();
  cJSON* pairing = ArenaPairingSnapshot();
  if (pairing) {
    pthread_mutex_lock(&mutex);
    cJSON_DeleteItemFromObject(snapshot, "pairing");
    cJSON_AddItemToObject(snapshot, "pairing", pairing);
    pthread_mutex_unlock(&mutex);
  }
#endif
  cJSON* monitoring = ArenaMonitoringSnapshot();
  pthread_mutex_lock(&mutex);
  cJSON_DeleteItemFromObject(snapshot, "monitoring");
  cJSON_AddItemToObject(snapshot, "monitoring", monitoring);
  cJSON* copy = cJSON_Duplicate(snapshot, true);
  pthread_mutex_unlock(&mutex);
  ArenaEmit("snapshot", NULL, copy);
}

static void command_loop(void) {
  // EOF means the Docker attachment closed, not that the native stack should
  // stop. Keep publishing observations while waiting for the shutdown signal.
  char line[65536];
  bool stdin_open = true;
  while (!stopping) {
    struct pollfd fd = {STDIN_FILENO, POLLIN, 0};
    int available = poll(&fd, stdin_open ? 1 : 0, 1000);
    if (available > 0 && (fd.revents & POLLIN) && fgets(line, sizeof(line), stdin)) {
      cJSON* cmd = cJSON_Parse(line);
      if (cmd) {
        handle(cmd);
        cJSON_Delete(cmd);
      } else
        ArenaEmit("command.invalid", NULL, NULL);
    }
    if (available > 0 && (fd.revents & (POLLHUP | POLLERR | POLLNVAL)))
      stdin_open = false;
    if (feof(stdin))
      stdin_open = false;
    publish_snapshot();
  }
}

static void stop_child(pid_t pid) {
  // A failed fork returns -1; passing that to kill would signal unrelated
  // processes. Only stop children this adapter actually started.
  if (pid <= 0)
    return;
  kill(pid, SIGTERM);
  while (waitpid(pid, NULL, 0) < 0 && errno == EINTR) {
  }
}

int main(void) {
  setvbuf(stdin, NULL, _IONBF, 0);
  // Native libraries log to stdout. Preserve the protocol descriptor first so
  // their diagnostics cannot become malformed NDJSON frames for the supervisor.
  protocol = fdopen(dup(STDOUT_FILENO), "w");
  if (!protocol || dup2(STDERR_FILENO, STDOUT_FILENO) < 0)
    return 1;
  setvbuf(stdout, NULL, _IOLBF, 0);
  if (!load_configuration())
    return 1;
  initialize_snapshot();
  signal(SIGINT, stop_signal);
  signal(SIGTERM, stop_signal);

  int status = 1;
  if (start_discovery_services() && start_native_stack()) {
    emit_capabilities();
    command_loop();
    status = 0;
  }
  if (hems)
    HemsClose(hems);
  if (hpsrv)
    HpsrvClose(hpsrv);
  stop_child(avahi_pid);
  stop_child(dbus_pid);
  return status;
}

#ifdef ARENA_PAIRING
// A discovery flood must not block native pairing on supervisor I/O. Report
// suppression explicitly so a test cannot mistake missing evidence for success.
void ArenaPairingTrace(const char* kind, cJSON* data) {
  static pthread_mutex_t trace_mutex = PTHREAD_MUTEX_INITIALIZER;
  static time_t window;
  static unsigned count, suppressed;
  pthread_mutex_lock(&trace_mutex);
  time_t now = time(NULL);
  if (now != window) {
    if (suppressed) {
      cJSON* gap = cJSON_CreateObject();
      cJSON_AddNumberToObject(gap, "count", suppressed);
      ArenaEmit("pairing.suppressed", NULL, gap);
    }
    window = now;
    count = 0;
    suppressed = 0;
  }
  if (count++ >= 100) {
    suppressed++;
    if (suppressed == 1) {
      cJSON* gap = cJSON_CreateObject();
      cJSON_AddNumberToObject(gap, "count", 1);
      cJSON_AddStringToObject(gap, "boundary", "started");
      ArenaEmit("pairing.suppressed", NULL, gap);
    }
    cJSON_Delete(data);
  } else
    ArenaEmit(kind, NULL, data);
  pthread_mutex_unlock(&trace_mutex);
}
#endif
