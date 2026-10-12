package org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.demo;

import java.io.OutputStream;
import java.io.PrintStream;
import java.util.Map;

/** Runs without a native device or network; checks callback/snapshot ownership. */
public final class MainTest {
  private MainTest() {}

  public static void main(String[] args) throws InterruptedException {
    // Set this before Main captures the protocol stream. Test observations are
    // intentionally discarded; a failed assertion still reaches stderr.
    System.setOut(new PrintStream(OutputStream.nullOutputStream()));
    Main.observe("usecase.limit", Map.of("usecase", "lpc", "value", 1200.0, "active", true));
    Map<String, Object> first = Main.snapshot();
    Main.updateLimit("lpc", "FAILSAFE", null);
    Map<?, ?> observed = (Map<?, ?>) Main.snapshot().get("lpc");
    assert observed.get("value").equals(1200.0) : "state update lost the observed limit";
    assert observed.get("state").equals("FAILSAFE");
    assert !((Map<?, ?>) first.get("lpc")).containsKey("state") : "published snapshot was mutated";

    Thread limits =
        new Thread(
            () -> {
              for (int i = 0; i < 1000; i++) {
                Main.observe("usecase.limit", Map.of("usecase", "lpc", "value", i));
              }
            });
    Thread states =
        new Thread(
            () -> {
              for (int i = 0; i < 1000; i++) {
                Main.updateLimit("lpc", "state-" + i, null);
              }
            });
    limits.start();
    states.start();
    limits.join();
    states.join();
    observed = (Map<?, ?>) Main.snapshot().get("lpc");
    assert observed.get("value").equals(999) : "state callback overwrote a newer limit";
    assert observed.get("state").equals("state-999") : "limit callback overwrote a newer state";
    assert !observed.containsKey("effectiveValue") : "inactive limit retained an effective value";
    System.err.println("Java adapter snapshot tests passed");
  }
}
