import { describe, expect, it } from "vitest";
import type { ControllerNode } from "./controller-api";
import { buildGatewayBootstrapSpec, formatPortRanges, parsePortRanges } from "./node-bootstrap";

const gateway: ControllerNode = {
  id: "gw-public",
  role: "gateway",
  name: "Public Gateway",
  labels: { site: "public" },
  enabled: true,
  certificate_state: "pending",
  revision: 1,
  created_at: "",
  updated_at: "",
};

describe("node bootstrap form helpers", () => {
  it("parses and formats port pools", () => {
    expect(parsePortRanges("28080-28082, 29000", "TCP")).toEqual([
      { min: 28080, max: 28082 },
      { min: 29000, max: 29000 },
    ]);
    expect(formatPortRanges([{ min: 28080, max: 28082 }, { min: 29000, max: 29000 }])).toBe("28080-28082,29000");
  });

  it("rejects malformed or reversed ranges", () => {
    expect(() => parsePortRanges("0-10", "UDP")).toThrow();
    expect(() => parsePortRanges("29000-28080", "UDP")).toThrow();
    expect(() => parsePortRanges("1-2-3", "UDP")).toThrow();
  });

  it("builds a valid minimal Gateway spec", () => {
    const spec = buildGatewayBootstrapSpec(gateway, "gateway.example.com:4433", "28080-28999", "28080-28999");
    expect(spec).toMatchObject({
      node_id: "gw-public",
      public_endpoints: ["gateway.example.com:4433"],
      labels: { site: "public" },
      transport: { alpn: "asterferry-data/2" },
    });
    expect(spec.port_pool).toEqual({
      tcp: [{ min: 28080, max: 28999 }],
      udp: [{ min: 28080, max: 28999 }],
    });
  });
});
