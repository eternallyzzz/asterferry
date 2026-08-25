import { beforeEach, describe, expect, it, vi } from "vitest";
import { APIError, consumeEventStream, fetchConfig, validateConfig } from "./api";

describe("dashboard API", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("parses authenticated SSE events and gap notifications", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      "id: 8\nevent: log\ndata: {\"id\":8,\"time\":\"2026-01-01T00:00:00Z\",\"level\":\"info\",\"event\":\"agent.connected\"}\n\n" +
      "event: gap\ndata: {\"from\":9,\"to\":10}\n\n",
      { status: 200, headers: { "content-type": "text/event-stream" } },
    ));
    vi.stubGlobal("fetch", fetchMock);
    const events: string[] = [];
    const gaps: string[] = [];
    const controller = new AbortController();
    await consumeEventStream("secret-token", 7, {
      onEvent: (event) => events.push(event.event + ":" + event.id),
      onGap: (from, to) => gaps.push(from + "-" + to),
    }, controller.signal);
    expect(events).toEqual(["agent.connected:8"]);
    expect(gaps).toEqual(["9-10"]);
    expect(fetchMock).toHaveBeenCalledWith("/v1/events", expect.objectContaining({
      headers: expect.any(Headers),
    }));
    const request = fetchMock.mock.calls[0][1] as RequestInit;
    const headers = request.headers as Headers;
    expect(headers.get("Authorization")).toBe("Bearer secret-token");
    expect(headers.get("Last-Event-ID")).toBe("7");
  });

  it("surfaces structured validation errors without leaking response objects", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ error: { code: "action_busy", message: "action is already in progress" } }),
      { status: 409, headers: { "content-type": "application/json" } },
    )));
    await expect(validateConfig("secret-token", { base_revision: "abc", config: { role: "agent" } })).rejects.toMatchObject({
      status: 409,
      code: "action_busy",
      message: "action is already in progress",
    } satisfies Partial<APIError>);
  });

  it("loads and validates redacted configuration documents", async () => {
    const fetchMock = vi.fn().mockImplementation(() => new Response(
      JSON.stringify({ schema_version: 1, role: "agent", revision: "abc", writable: true, backup_available: false, yaml: "role: agent", values: { role: "agent" } }),
      { status: 200, headers: { "content-type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);
    const snapshot = await fetchConfig("secret-token");
    expect(snapshot.role).toBe("agent");
    await validateConfig("secret-token", { base_revision: "abc", config: { role: "agent" } });
    const request = fetchMock.mock.calls[1][1] as RequestInit;
    const headers = request.headers as Headers;
    expect(headers.get("Authorization")).toBe("Bearer secret-token");
    expect(headers.get("Content-Type")).toBe("application/json");
    expect(String(request.body)).toContain("base_revision");
  });
});
