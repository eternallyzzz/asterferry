import { afterEach, describe, expect, it, vi } from "vitest";
import { getNodeRuntimeConnection } from "./controller-api";

describe("controller runtime API", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("gets one runtime connection with both path segments encoded", async () => {
    const connection = { id: "conn/1", node_id: "node one", state: "active" };
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => connection,
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(getNodeRuntimeConnection("node one", "conn/1")).resolves.toEqual(connection);

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/nodes/node%20one/runtime/connections/conn%2F1",
      expect.objectContaining({ credentials: "include", cache: "no-store" }),
    );
  });
});
