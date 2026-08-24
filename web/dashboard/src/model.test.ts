import { describe, expect, it } from "vitest";
import { appendPoint, counterRate, formatBytes, formatDuration, statusLabel } from "./model";

describe("dashboard model", () => {
  it("formats bytes and durations for compact cards", () => {
    expect(formatBytes(1024)).toBe("1.00 KiB");
    expect(formatBytes(1024 * 1024 * 2)).toBe("2.00 MiB");
    expect(formatDuration(250)).toBe("250 ms");
    expect(formatDuration(2500)).toBe("2.5 s");
  });

  it("calculates monotonic counter rates and guards resets", () => {
    expect(counterRate(300, 100, 2000)).toBe(100);
    expect(counterRate(10, 100, 1000)).toBe(0);
    expect(counterRate(100, 0, 0)).toBe(0);
  });

  it("keeps a bounded trend history", () => {
    expect(appendPoint([{ time: 1, value: 1 }, { time: 2, value: 2 }], { time: 3, value: 3 }, 2)).toEqual([
      { time: 2, value: 2 },
      { time: 3, value: 3 },
    ]);
  });

  it("labels lifecycle state before readiness", () => {
    expect(statusLabel({ ready: true, state: "running" })).toBe("Operational");
    expect(statusLabel({ ready: false, state: "running" })).toBe("Degraded");
    expect(statusLabel({ ready: true, state: "draining" })).toBe("Draining");
  });
});
