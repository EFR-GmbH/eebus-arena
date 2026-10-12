package org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.demo;

import static org.openmuc.jeebus.shipspine.ShipCommunication.ConnectClientsTo.TRUSTED;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.PrintStream;
import java.net.InetSocketAddress;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyFactory;
import java.security.KeyStore;
import java.security.cert.CertificateFactory;
import java.security.spec.PKCS8EncodedKeySpec;
import java.time.Instant;
import java.util.Base64;
import java.util.Collections;
import java.util.HashMap;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import org.openmuc.jeebus.ship.api.ConfigBuilder;
import org.openmuc.jeebus.ship.api.cert.KeyStoreCertificateStorage;
import org.openmuc.jeebus.ship.node.ShipConfig;
import org.openmuc.jeebus.shipspine.ArenaObserver;
import org.openmuc.jeebus.shipspine.ShipCommunication;
import org.openmuc.jeebus.spine.api.Device;
import org.openmuc.jeebus.spine.utils.datatypes.ScaledNumberWrapper;
import org.openmuc.jeebus.spine.xsd.v1.DeviceTypeEnumType;
import org.openmuc.jeebus.spine.xsd.v1.EntityTypeEnumType;
import org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.ActiveLimit;
import org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.LimitationUseCaseImpl;
import org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.SimpleLimitationConfig;
import org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.lpc.LpcCs;
import org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.lpp.LppCs;

public final class Main {
  private Main() {}

  private static final ObjectMapper JSON = new ObjectMapper();
  // Reserve the original stdout for NDJSON before redirecting native library logs.
  private static final PrintStream PROTOCOL = System.out;
  private static final String INCARNATION = UUID.randomUUID().toString();
  private static long sequence;
  private static String nodeId;
  private static final Map<String, Object> peers = new ConcurrentHashMap<>();
  private static final Map<String, Object> state = new ConcurrentHashMap<>();

  private static final Map<String, Map<String, Object>> usecaseState = new ConcurrentHashMap<>();

  // Callback and scheduler threads share one ordered event stream.
  private static synchronized void emit(String type, String commandId, Map<String, Object> data) {
    try {
      Map<String, Object> event = new LinkedHashMap<>();
      event.put("type", type);
      event.put("nodeId", nodeId);
      event.put("incarnation", INCARNATION);
      event.put("sequence", ++sequence);
      event.put("at", Instant.now().toString());
      event.put("data", data);
      if (commandId != null) event.put("commandId", commandId);
      PROTOCOL.println(JSON.writeValueAsString(event));
      PROTOCOL.flush();
    } catch (IOException error) {
      error.printStackTrace(System.err);
    }
  }

  private static void result(String id, Exception error) {
    Map<String, Object> data = new HashMap<>();
    data.put("ok", error == null);
    if (error != null) data.put("error", error.getMessage());
    emit("command.result", id, data);
  }

  private static String createIdentityStore(JsonNode config) throws Exception {
    // SHIP expects a key store; the supervisor provisions PEM files shared by all stacks.
    java.security.cert.Certificate cert;
    try (var input = Files.newInputStream(Path.of(config.get("certificate").asText()))) {
      cert = CertificateFactory.getInstance("X.509").generateCertificate(input);
    }
    String text =
        Files.readString(Path.of(config.get("privateKey").asText()))
            .replace("-----BEGIN PRIVATE KEY-----", "")
            .replace("-----END PRIVATE KEY-----", "")
            .replaceAll("\\s", "");
    var key =
        KeyFactory.getInstance("EC")
            .generatePrivate(new PKCS8EncodedKeySpec(Base64.getDecoder().decode(text)));
    var store = KeyStore.getInstance("PKCS12");
    store.load(null, null);
    store.setKeyEntry(
        "arena", key, "arena-test".toCharArray(), new java.security.cert.Certificate[] {cert});
    String path = "/tmp/arena-identity.p12";
    try (var stream = Files.newOutputStream(Path.of(path))) {
      store.store(stream, "arena-test".toCharArray());
    }
    return path;
  }

  public static void main(String[] args) throws Exception {
    System.setOut(System.err);
    JsonNode config = JSON.readTree(Files.readString(Path.of("/identity/config.json")));
    nodeId = config.get("nodeId").asText();
    int scale = config.get("timeScale").asInt(1);
    System.setProperty("arena.timeScale", Integer.toString(scale));
    Set<String> trusted = new HashSet<>();
    config.get("trustedSkis").forEach(s -> trusted.add(s.asText()));
    state.put("peers", peers);
    state.put("stateSource", "observed");
    state.put("timeScale", scale);
    ArenaObserver.observer = Main::observe;
    ShipCommunication communication = createCommunication(config, trusted);
    var max = new ScaledNumberWrapper(10000, 0);
    var failsafe = new ScaledNumberWrapper(1000, 0);
    LpcCs lpc = new LpcCs(new SimpleLimitationConfig("PT2H", max, max, failsafe));
    LppCs lpp = new LppCs(new SimpleLimitationConfig("PT2H", max, max.negate(), failsafe));
    observeUseCase("lpc", lpc);
    observeUseCase("lpp", lpp);
    Device device =
        Device.getBuilder()
            .withDeviceType(DeviceTypeEnumType.GENERIC)
            .withCommunication(communication)
            .withId("d:_n:Arena_" + nodeId)
            .addEntity()
            .setType(EntityTypeEnumType.CEM)
            .withUseCases(lpc, lpp)
            .applyToDevice()
            .build();
    state.put("localSki", communication.getOwnSki());
    emitCapabilities();
    ScheduledExecutorService scheduler = startSnapshots(lpc, lpp);
    Runtime.getRuntime()
        .addShutdownHook(
            new Thread(
                () -> {
                  scheduler.shutdownNow();
                  try {
                    device.close();
                  } catch (Exception error) {
                    System.err.println("Native device shutdown failed: " + error);
                  }
                }));
    readCommands(lpc, lpp);
    // Container stdin remains open between Docker attachments; do not shut down on a reconnect.
    new CountDownLatch(1).await();
  }

  private static ShipCommunication createCommunication(JsonNode config, Set<String> trusted)
      throws Exception {
    String networkMode = config.path("networkMode").asText("ipv4");
    if (!Set.of("ipv4", "ipv6", "dual-stack").contains(networkMode))
      throw new IllegalArgumentException("Invalid networkMode");
    state.put("networkMode", networkMode);
    ConfigBuilder shipConfig =
        ShipConfig.getBuilder()
            .withNetworkInterfaceScanInitialDelay(0)
            .withServerBindAddresses(
                Collections.singleton(
                    new InetSocketAddress(
                        networkMode.equals("ipv4") ? "0.0.0.0" : "::", config.get("port").asInt())))
            .withId("Arena-" + nodeId)
            .withMDnsServiceInstance(nodeId)
            .withCertificateStorage(
                new KeyStoreCertificateStorage(
                    createIdentityStore(config),
                    "arena",
                    "arena-test".toCharArray(),
                    "arena-test".toCharArray()))
            .withCertificateDistinguishedName("CN=" + nodeId)
            .withTrustedSkis(trusted);
    return new ShipCommunication(shipConfig.build()).withConnectClientsTo(TRUSTED);
  }

  static Map<String, Object> snapshot() {
    Map<String, Object> snapshot = new HashMap<>(state);
    snapshot.putAll(usecaseState);
    return snapshot;
  }

  static void observe(String type, Map<String, Object> data) {
    if (type.equals("ship.state")) {
      if (data.get("ski") == null) {
        return;
      }
      Object previous = peers.put(data.get("ski").toString(), data.get("state"));
      // Native callbacks repeat connected state for every datagram; emit transitions only.
      if (Objects.equals(previous, data.get("state"))) {
        return;
      }
    }
    if (type.equals("usecase.limit")) {
      String usecase = data.get("usecase").toString();
      usecaseState.compute(
          usecase,
          (key, previous) -> {
            Map<String, Object> current = new HashMap<>(previous == null ? Map.of() : previous);
            current.putAll(data);
            return current;
          });
    }
    emit(type, null, data);
  }

  static void updateLimit(String usecase, String nativeState, ActiveLimit limit) {
    // Publish a fresh map: the snapshot encoder may still be reading the previous one.
    usecaseState.compute(
        usecase,
        (key, previous) -> {
          Map<String, Object> data = new HashMap<>(previous == null ? Map.of() : previous);
          data.put("state", nativeState);
          if (limit == null) {
            data.remove("effectiveValue");
          } else {
            data.put("effectiveValue", limit.getResultingValue());
          }
          return data;
        });
  }

  private static void observeUseCase(String name, LimitationUseCaseImpl usecase) {
    usecase.addListener(
        (event, current, limit) -> {
          updateLimit(name, current.name(), limit);
          emit(
              "usecase.state",
              null,
              Map.of("usecase", name, "state", current.name(), "event", event.name()));
        });
  }

  private static ScheduledExecutorService startSnapshots(LpcCs lpc, LppCs lpp) {
    ScheduledExecutorService scheduler = Executors.newSingleThreadScheduledExecutor();
    scheduler.scheduleAtFixedRate(
        () -> {
          updateLimit("lpc", lpc.getState().name(), lpc.getActiveLimit());
          updateLimit("lpp", lpp.getState().name(), lpp.getActiveLimit());
          emit("snapshot", null, snapshot());
        },
        0,
        1,
        TimeUnit.SECONDS);
    return scheduler;
  }

  private static void emitCapabilities() {
    emit(
        "capabilities",
        null,
        Map.of(
            "protocolVersion",
            1,
            "commands",
            List.of(
                "snapshot",
                "heartbeat.set",
                "heartbeat.interval",
                "observe.limit",
                "observe.state",
                "observe.ship",
                "observe.spine",
                "timing.accelerated")));
  }

  private static void readCommands(LpcCs lpc, LppCs lpp) throws IOException {
    BufferedReader input = new BufferedReader(new InputStreamReader(System.in));
    String line;
    while ((line = input.readLine()) != null) {
      JsonNode command;
      try {
        command = JSON.readTree(line);
      } catch (IOException error) {
        emit("command.invalid", null, Map.of("error", error.toString()));
        continue;
      }
      handleCommand(command, lpc, lpp);
    }
  }

  private static void handleCommand(JsonNode command, LpcCs lpc, LppCs lpp) {
    String id = command.path("id").asText();
    try {
      switch (command.path("command").asText()) {
        case "snapshot" -> emit("snapshot", null, snapshot());
        case "heartbeat.set" -> {
          if (!command.path("params").path("enabled").isBoolean())
            throw new IllegalArgumentException("enabled must be a boolean");
          boolean enabled = command.path("params").path("enabled").asBoolean();
          lpc.getHeartbeatDataFunction().arenaSetEnabled(enabled);
          lpp.getHeartbeatDataFunction().arenaSetEnabled(enabled);
        }
        case "heartbeat.interval" -> {
          long seconds = command.path("params").path("seconds").asLong();
          if (seconds < 1 || seconds > 3600)
            throw new IllegalArgumentException("seconds must be 1–3600");
          lpc.getHeartbeatDataFunction().setTimeoutSeconds(seconds);
          lpp.getHeartbeatDataFunction().setTimeoutSeconds(seconds);
        }
        default -> throw new IllegalArgumentException("Unsupported command");
      }
      result(id, null);
    } catch (Exception error) {
      result(id, error);
    }
  }
}
