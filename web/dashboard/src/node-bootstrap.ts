import type { ControllerNode } from "./controller-api";

export interface PortRange {
  min: number;
  max: number;
}

export function parsePortRanges(value: string, field: string): PortRange[] {
  const text = value.trim();
  if (!text) return [];
  return text.split(",").map((part) => {
    const pieces = part.trim().split("-").map((item) => Number(item.trim()));
    if (pieces.length > 2 || pieces.some((item) => !Number.isInteger(item) || item < 1 || item > 65535)) {
      throw new Error(`${field} 必须是端口或端口范围，例如 28080-28999`);
    }
    const min = pieces[0];
    const max = pieces[1] ?? min;
    if (max < min) throw new Error(`${field} 的范围起止端口无效`);
    return { min, max };
  });
}

export function formatPortRanges(value: unknown): string {
  if (!Array.isArray(value)) return "";
  return value
    .filter((item): item is { min?: unknown; max?: unknown } => Boolean(item) && typeof item === "object")
    .map((item) => {
      const min = Number(item.min);
      const max = Number(item.max ?? item.min);
      return Number.isInteger(min) && Number.isInteger(max) && min > 0 && max >= min ? (min === max ? String(min) : `${min}-${max}`) : "";
    })
    .filter(Boolean)
    .join(",");
}

export function buildGatewayBootstrapSpec(node: Pick<ControllerNode, "id" | "labels">, endpoint: string, tcpPool: string, udpPool: string): Record<string, unknown> {
  const publicEndpoint = endpoint.trim();
  if (!publicEndpoint) throw new Error("Gateway 公网 AFDP 地址不能为空，例如 gateway.example.com:4433");
  return {
    node_id: node.id,
    public_endpoints: [publicEndpoint],
    listeners: [],
    labels: node.labels || {},
    capacity: { max_agents: 128, max_connections: 4096, max_services: 4096 },
    port_pool: { tcp: parsePortRanges(tcpPool, "TCP 端口池"), udp: parsePortRanges(udpPool, "UDP 端口池") },
    transport: { alpn: "asterferry-data/2", max_streams: 1024, max_frame_bytes: 65536, max_datagram_bytes: 65536, handshake_timeout_seconds: 10, idle_timeout_seconds: 300 },
    obfuscation: { mode: "standard", max_padding_bytes: 0, handshake_shaping: false },
    egress: { enabled: false, max_connections: 0 },
  };
}
