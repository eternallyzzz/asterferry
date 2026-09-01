import { describe, expect, it } from "vitest";
import { ControllerAPIError } from "../controller-api";
import {
  certificateTone,
  copyText,
  describeError,
  formatTime,
  joinList,
  newIdempotencyKey,
  parseLabels,
  parseObject,
  prettyJson,
  roleLabel,
  splitList,
  stateLabel,
  userRoleLabel,
} from "./format";

describe("formatTime", () => {
  it("formats ISO timestamps in zh-CN without 12-hour clock", () => {
    const result = formatTime("2026-08-31T10:20:30Z");
    expect(result).not.toBe("—");
    expect(result).toContain("2026");
  });

  it("returns a dash for empty or invalid input", () => {
    expect(formatTime("")).toBe("—");
    expect(formatTime(undefined)).toBe("—");
    expect(formatTime("not-a-date")).toBe("—");
  });
});

describe("label helpers", () => {
  it("maps node roles", () => {
    expect(roleLabel("gateway")).toBe("Gateway");
    expect(roleLabel("agent")).toBe("Agent");
    expect(roleLabel("other")).toBe("other");
  });

  it("maps user roles", () => {
    expect(userRoleLabel("admin")).toContain("Admin");
    expect(userRoleLabel("viewer")).toContain("Viewer");
    expect(userRoleLabel("operator")).toContain("Operator");
  });

  it("maps assignment states", () => {
    expect(stateLabel("applied")).toBe("已应用");
    expect(stateLabel("pending")).toBe("等待");
    expect(stateLabel("")).toBe("未知");
  });

  it("maps certificate tones", () => {
    expect(certificateTone("active")).toBe("good");
    expect(certificateTone("revoked")).toBe("bad");
    expect(certificateTone("expired")).toBe("bad");
    expect(certificateTone("pending")).toBe("warn");
  });
});

describe("describeError", () => {
  it("explains revision conflicts and duplicates", () => {
    expect(describeError(new ControllerAPIError(409, "conflict", "revision_conflict"))).toContain("revision");
    expect(describeError(new ControllerAPIError(409, "conflict", "already_exists"))).toContain("已存在");
  });

  it("explains permission and missing resources", () => {
    expect(describeError(new ControllerAPIError(403, "forbidden"))).toContain("权限");
    expect(describeError(new ControllerAPIError(404, "missing"))).toContain("不存在");
  });

  it("explains how to fix missing Controller bootstrap address", () => {
    expect(describeError(new ControllerAPIError(503, "controller grpc_advertise must be a reachable host:port before generating node installation commands", "bootstrap_unavailable"))).toContain("grpc_advertise");
  });

  it("falls back to the message for other errors", () => {
    expect(describeError(new ControllerAPIError(500, "boom"))).toBe("boom");
    expect(describeError(new Error("plain"))).toBe("plain");
    expect(describeError("weird")).toBe("Controller 请求失败。");
  });
});

describe("json helpers", () => {
  it("pretty-prints JSON and tolerates circular values", () => {
    expect(prettyJson({ a: 1 })).toBe('{\n  "a": 1\n}');
    const circular: Record<string, unknown> = {};
    circular.self = circular;
    expect(prettyJson(circular)).toBe("—");
  });

  it("parses objects and rejects invalid shapes", () => {
    expect(parseObject('{"a":1}', "spec")).toEqual({ a: 1 });
    expect(() => parseObject("{oops", "spec")).toThrow("有效 JSON");
    expect(() => parseObject("[1]", "spec")).toThrow("JSON 对象");
  });

  it("parses labels with string values only", () => {
    expect(parseLabels('{"region":"east"}')).toEqual({ region: "east" });
    expect(() => parseLabels('{"region":1}')).toThrow("字符串");
  });
});

describe("list helpers", () => {
  it("joins and splits comma/newline separated values", () => {
    expect(joinList(["443", "8000-8080"])).toBe("443, 8000-8080");
    expect(joinList(undefined)).toBe("");
    expect(splitList("443, 8000-8080\n53")).toEqual(["443", "8000-8080", "53"]);
    expect(splitList("  ")).toEqual([]);
  });
});

describe("newIdempotencyKey", () => {
  it("produces unique non-empty keys", () => {
    const first = newIdempotencyKey();
    const second = newIdempotencyKey();
    expect(first).toBeTruthy();
    expect(second).toBeTruthy();
    expect(first).not.toBe(second);
  });
});

describe("copyText", () => {
  it("resolves a boolean instead of throwing", async () => {
    await expect(copyText("value")).resolves.toBeTypeOf("boolean");
  });
});
