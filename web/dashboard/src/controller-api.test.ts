import { afterEach, describe, expect, it, vi } from "vitest";
import { getNodeRuntimeConnection, listRuntimeConnections, updateNode } from "./controller-api";

describe("controller runtime API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("gets one runtime connection with both path segments encoded", async () => {
    const connection = { id: "conn/1", node_id: "node one", state: "active" };
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify(connection),
      { status: 200, headers: { "content-type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);

    await expect(getNodeRuntimeConnection("node one", "conn/1")).resolves.toEqual(connection);

    const request = fetchMock.mock.calls[0]?.[0] as Request;
    expect(request.url).toContain("/api/v1/nodes/node%20one/runtime/connections/conn%2F1");
    expect(request.credentials).toBe("include");
  });

  it("applies shared authentication, CAS and idempotency headers", async () => {
    const node = { id: "node/1", name: "Node", enabled: true };
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify(node),
      { status: 200, headers: { "content-type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);
    document.cookie = "af_csrf=test-csrf";

    await expect(updateNode("node/1", { enabled: false }, 7, "api-token", "request-1")).resolves.toEqual(node);

    const request = fetchMock.mock.calls[0]?.[0] as Request;
    expect(request.method).toBe("PATCH");
    expect(request.headers.get("authorization")).toBe("Bearer api-token");
    expect(request.headers.get("if-match")).toBe("7");
    expect(request.headers.get("idempotency-key")).toBe("request-1");
    expect(request.headers.get("x-csrf-token")).toBe("test-csrf");
    expect(request.headers.get("content-type")).toContain("application/json");
  });

  it("keeps the documented global runtime filters in the generated query type", async () => {
    const result = { items: [] };
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify(result),
      { status: 200, headers: { "content-type": "application/json" } },
    ));
    vi.stubGlobal("fetch", fetchMock);

    await expect(listRuntimeConnections(
      "node one",
      "type=udp&peer_node_id=peer one&gateway_id=gw&agent_id=agent&protocol=quic&limit=10",
    )).resolves.toEqual(result);

    const request = fetchMock.mock.calls[0]?.[0] as Request;
    const query = new URL(request.url).searchParams;
    expect(query.get("node_id")).toBe("node one");
    expect(query.get("type")).toBe("udp");
    expect(query.get("peer_node_id")).toBe("peer one");
    expect(query.get("gateway_id")).toBe("gw");
    expect(query.get("agent_id")).toBe("agent");
    expect(query.get("protocol")).toBe("quic");
    expect(query.get("limit")).toBe("10");
  });
});
