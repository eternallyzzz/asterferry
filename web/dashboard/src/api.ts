export type Role = "gateway" | "agent";

export interface MetricsSnapshot {
  connections: number;
  active_streams: number;
  draining: boolean;
  shutdowns_total: number;
  forced_shutdowns_total: number;
  bytes_in_total: number;
  bytes_out_total: number;
  auth_failures_total: number;
  mapping_failures_total: number;
  obfuscation_packets_accepted_total: number;
  obfuscation_packets_rejected_total: number;
  obfuscation_previous_key_total: number;
  obfuscation_fragments_dropped_total: number;
  quic: {
    rtt_microseconds: number;
    bytes_sent: number;
    bytes_received: number;
    bytes_lost: number;
    packets_sent: number;
    packets_received: number;
    packets_lost: number;
    gso: boolean;
    stats_samples: number;
  };
}

export interface DashboardSnapshot {
  schema_version: number;
  generated_at: string;
  role: Role;
  state: string;
  ready: boolean;
  node_id: string;
  transport: {
    protocol: number;
    obfuscation_mode: string;
    key_fingerprint?: string;
  };
  metrics: MetricsSnapshot;
  gateway?: {
    agents: Array<{
      agent_id: string;
      session_id: string;
      node_id: string;
      connected: boolean;
      mapping_count: number;
    }>;
    mappings: Array<{
      name: string;
      agent_id: string;
      protocol: string;
      gateway_port: number;
      profile: string;
      state: string;
    }>;
  };
  agent?: {
    agent_id: string;
    connected: boolean;
    session_id?: string;
    reconnects: number;
    inbounds: Array<{ tag: string; protocol: string; listen: string }>;
    reverse_mappings: Array<{ name: string; protocol: string; gateway_port: number; local: string }>;
  };
}

export interface DashboardEvent {
  id: number;
  time: string;
  level: string;
  event: string;
  role?: string;
  security_audit?: boolean;
  attributes?: Record<string, string>;
}

export class APIError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.code = code;
  }
}

async function request<T>(token: string, path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Authorization", "Bearer " + token);
  headers.set("Accept", "application/json");
  const response = await fetch(path, { ...init, headers, cache: "no-store" });
  if (!response.ok) {
    let message = "request failed with HTTP " + response.status;
    let code: string | undefined;
    try {
      const body = (await response.json()) as { error?: { code?: string; message?: string } };
      message = body.error?.message || message;
      code = body.error?.code;
    } catch {
      // Keep the stable HTTP error when the response is not JSON.
    }
    throw new APIError(response.status, message, code);
  }
  return (await response.json()) as T;
}

export function fetchSnapshot(token: string): Promise<DashboardSnapshot> {
  return request<DashboardSnapshot>(token, "/v1/dashboard");
}

export async function requestAction(token: string, action: "shutdown" | "reconnect"): Promise<void> {
  await request<{ action: string }>(token, "/v1/actions/" + action, { method: "POST" });
}

export interface EventCallbacks {
  onOpen?(): void;
  onEvent(event: DashboardEvent): void;
  onGap(from: number, to: number): void;
}

export async function consumeEventStream(
  token: string,
  lastEventID: number,
  callbacks: EventCallbacks,
  signal: AbortSignal,
): Promise<void> {
  const headers = new Headers({ Authorization: "Bearer " + token, Accept: "text/event-stream" });
  if (lastEventID > 0) headers.set("Last-Event-ID", String(lastEventID));
  const response = await fetch("/v1/events", { headers, cache: "no-store", signal });
  if (!response.ok) {
    throw new APIError(response.status, "event stream failed with HTTP " + response.status);
  }
  if (!response.body) throw new Error("event stream has no response body");
  callbacks.onOpen?.();

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let eventName = "message";
  let eventID = 0;
  let data: string[] = [];

  const dispatch = () => {
    if (data.length === 0) return;
    const raw = data.join("\n");
    data = [];
    if (eventName === "gap") {
      try {
        const gap = JSON.parse(raw) as { from?: number; to?: number };
        if (typeof gap.from === "number" && typeof gap.to === "number") callbacks.onGap(gap.from, gap.to);
      } catch {
        // Ignore malformed non-data control events.
      }
    } else {
      try {
        const event = JSON.parse(raw) as DashboardEvent;
        if (eventID > 0) event.id = eventID;
        callbacks.onEvent(event);
      } catch {
        // Ignore malformed events; the next valid event remains usable.
      }
    }
    eventName = "message";
    eventID = 0;
  };

  const consumeLines = (text: string) => {
    buffer += text;
    const lines = buffer.split("\n");
    buffer = lines.pop() || "";
    for (const rawLine of lines) {
      const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
      if (line === "") {
        dispatch();
        continue;
      }
      if (line.startsWith(":")) continue;
      const separator = line.indexOf(":");
      const field = separator < 0 ? line : line.slice(0, separator);
      const value = separator < 0 ? "" : line.slice(separator + 1).replace(/^ /, "");
      if (field === "event") eventName = value;
      else if (field === "id") eventID = Number(value) || 0;
      else if (field === "data") data.push(value);
    }
  };

  try {
    while (true) {
      const result = await reader.read();
      if (result.done) {
        consumeLines(decoder.decode());
        dispatch();
        return;
      }
      consumeLines(decoder.decode(result.value, { stream: true }));
    }
  } finally {
    reader.releaseLock();
  }
}
