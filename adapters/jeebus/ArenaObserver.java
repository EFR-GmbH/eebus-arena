package org.openmuc.jeebus.shipspine;

import java.util.LinkedHashMap;
import java.util.Map;
import java.util.function.BiConsumer;

/** Instrumentation callback only; protocol decisions remain in upstream code. */
public final class ArenaObserver {
  private ArenaObserver() {}

  public static volatile BiConsumer<String, Map<String, Object>> observer = (type, data) -> {};

  public static void emit(String type, Map<String, Object> data) {
    try {
      observer.accept(type, data);
    } catch (RuntimeException error) {
      // An instrumentation failure must never change the native protocol outcome.
      System.err.println("Arena observation failed: " + error);
    }
  }

  public static Map<String, Object> data(Object... fields) {
    Map<String, Object> result = new LinkedHashMap<>();
    for (int i = 0; i < fields.length; i += 2) result.put((String) fields[i], fields[i + 1]);
    return result;
  }
}
