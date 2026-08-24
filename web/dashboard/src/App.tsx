import { useEffect, useMemo, useRef, useState } from "react";
import {
  APIError,
  consumeEventStream,
  fetchSnapshot,
  requestAction,
  type DashboardEvent,
  type DashboardSnapshot,
} from "./api";
import {
  appendPoint,
  counterRate,
  formatBytes,
  formatDuration,
  formatRate,
  snapshotErrorTotal,
  statusLabel,
  type MetricPoint,
} from "./model";

type Theme = "dark" | "light";
type ActionName = "shutdown" | "reconnect";

interface TrendState {
  input: MetricPoint[];
  output: MetricPoint[];
  errors: MetricPoint[];
}

const emptyTrend: TrendState = { input: [], output: [], errors: [] };

export default function App() {
  const [token, setToken] = useState("");
  const [snapshot, setSnapshot] = useState<DashboardSnapshot | null>(null);
  const [events, setEvents] = useState<DashboardEvent[]>([]);
  const [trend, setTrend] = useState<TrendState>(emptyTrend);
  const [error, setError] = useState("");
  const [streamState, setStreamState] = useState("offline");
  const [action, setAction] = useState<ActionName | null>(null);
  const [theme, setTheme] = useState<Theme>(() => {
    const saved = window.localStorage.getItem("asterferry.dashboard.theme");
    return saved === "light" ? "light" : "dark";
  });
  const lastEventID = useRef(0);
  const previous = useRef<{ snapshot: DashboardSnapshot; time: number } | null>(null);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem("asterferry.dashboard.theme", theme);
  }, [theme]);

  useEffect(() => {
    if (!token) {
      setSnapshot(null);
      setEvents([]);
      setTrend(emptyTrend);
      setStreamState("offline");
      previous.current = null;
      return;
    }
    const controller = new AbortController();
    let active = true;

    const applySnapshot = (next: DashboardSnapshot) => {
      const now = Date.now();
      const old = previous.current;
      if (old) {
        const elapsed = now - old.time;
        const oldMetrics = old.snapshot.metrics;
        setTrend((current) => ({
          input: appendPoint(current.input, { time: now, value: counterRate(next.metrics.bytes_in_total, oldMetrics.bytes_in_total, elapsed) }),
          output: appendPoint(current.output, { time: now, value: counterRate(next.metrics.bytes_out_total, oldMetrics.bytes_out_total, elapsed) }),
          errors: appendPoint(current.errors, { time: now, value: counterRate(snapshotErrorTotal(next.metrics), snapshotErrorTotal(oldMetrics), elapsed) }),
        }));
      }
      previous.current = { snapshot: next, time: now };
      setSnapshot(next);
      setError("");
    };

    const refresh = async () => {
      try {
        applySnapshot(await fetchSnapshot(token));
      } catch (caught) {
        if (!active) return;
        if (caught instanceof APIError && caught.status === 401) {
          setError("Token rejected. Enter the management token again.");
          setToken("");
          return;
        }
        setError(caught instanceof Error ? caught.message : "Dashboard refresh failed.");
      }
    };

    const wait = (ms: number) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));
    const stream = async () => {
      while (active && !controller.signal.aborted) {
        try {
          setStreamState("connecting");
          await consumeEventStream(
            token,
            lastEventID.current,
            {
              onOpen: () => setStreamState("connected"),
              onEvent: (event) => {
                lastEventID.current = Math.max(lastEventID.current, event.id);
                setEvents((current) => [event, ...current].slice(0, 80));
              },
              onGap: (from, to) => {
                setEvents((current) => [
                  {
                    id: to,
                    time: new Date().toISOString(),
                    level: "warn",
                    event: "events.gap",
                    attributes: { from: String(from), to: String(to) },
                  },
                  ...current,
                ].slice(0, 80));
              },
            },
            controller.signal,
          );
          if (active) setStreamState("reconnecting");
        } catch (caught) {
          if (!active || controller.signal.aborted) return;
          if (caught instanceof APIError && caught.status === 401) {
            setError("Token rejected. Enter the management token again.");
            setToken("");
            return;
          }
          setStreamState("reconnecting");
        }
        await wait(2000);
      }
    };

    void refresh();
    const refreshTimer = window.setInterval(() => void refresh(), 5000);
    void stream();
    return () => {
      active = false;
      controller.abort();
      window.clearInterval(refreshTimer);
    };
  }, [token]);

  const runAction = async (name: ActionName) => {
    const label = name === "shutdown" ? "gracefully stop this AsterFerry process" : "force the Agent to reconnect";
    if (!window.confirm("Confirm: " + label + "?")) return;
    setAction(name);
    try {
      await requestAction(token, name);
      setError(name === "shutdown" ? "Shutdown requested. The process will drain and exit." : "Reconnect requested.");
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Action failed.");
    } finally {
      setAction(null);
    }
  };

  if (!token) {
    return <TokenGate onSubmit={setToken} error={error} theme={theme} onTheme={() => setTheme(theme === "dark" ? "light" : "dark")} />;
  }
  return (
    <main className="shell">
      <header className="topbar">
        <div>
          <p className="eyebrow">ASTERFERRY / OPERATIONS</p>
          <h1>Transport command center</h1>
        </div>
        <div className="topbar-actions">
          <span className={"stream-dot " + streamState} title={"Event stream: " + streamState} />
          <button className="quiet-button" onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>{theme === "dark" ? "Light mode" : "Dark mode"}</button>
          <button className="quiet-button" onClick={() => setToken("")}>Lock</button>
        </div>
      </header>
      {error && <div className="notice error">{error}</div>}
      {snapshot ? (
        <DashboardView snapshot={snapshot} trend={trend} events={events} action={action} onAction={runAction} />
      ) : (
        <div className="loading-card"><span className="spinner" /> Loading runtime snapshot…</div>
      )}
    </main>
  );
}

function TokenGate(props: { onSubmit: (value: string) => void; error: string; theme: Theme; onTheme: () => void }) {
  const [draft, setDraft] = useState("");
  const submit = (event: React.FormEvent) => {
    event.preventDefault();
    if (draft.trim()) props.onSubmit(draft.trim());
  };
  return (
    <main className="auth-shell">
      <section className="auth-card">
        <div className="brand-mark">AF</div>
        <p className="eyebrow">ASTERFERRY / PRIVATE MANAGEMENT</p>
        <h1>Open the command center</h1>
        <p className="muted">The dashboard stays on the local management listener. Your Bearer token is held in memory and is never placed in the URL or saved on disk.</p>
        <form onSubmit={submit}>
          <label htmlFor="token">Management token</label>
          <input id="token" type="password" autoFocus value={draft} onChange={(event) => setDraft(event.target.value)} placeholder="Paste the token from management.token" />
          <button className="primary-button" type="submit">Unlock dashboard</button>
        </form>
        {props.error && <p className="form-error">{props.error}</p>}
        <button className="text-button" onClick={props.onTheme}>Use {props.theme === "dark" ? "light" : "dark"} theme</button>
      </section>
    </main>
  );
}

function DashboardView(props: { snapshot: DashboardSnapshot; trend: TrendState; events: DashboardEvent[]; action: ActionName | null; onAction: (name: ActionName) => void }) {
  const { snapshot } = props;
  const metrics = snapshot.metrics;
  const agents = snapshot.gateway?.agents.length ?? 0;
  const mappings = snapshot.gateway?.mappings.length ?? snapshot.agent?.reverse_mappings.length ?? 0;
  const inputRate = latest(props.trend.input);
  const outputRate = latest(props.trend.output);
  const errorRate = latest(props.trend.errors);
  const canReconnect = snapshot.role === "agent" && snapshot.state === "running";
  const canShutdown = snapshot.state === "running";

  return (
    <>
      <section className="hero-grid">
        <div className="hero-card">
          <div className="status-line"><span className={"status-light " + (snapshot.ready ? "good" : "warn")} /><span>{statusLabel(snapshot)}</span></div>
          <h2>{snapshot.role === "gateway" ? "Gateway" : "Agent"} runtime</h2>
          <p className="muted">Node <code>{snapshot.node_id || "local"}</code> · Protocol v{snapshot.transport.protocol} · {snapshot.transport.obfuscation_mode}</p>
          <div className="hero-actions">
            {canReconnect && <button className="secondary-button" disabled={props.action !== null} onClick={() => props.onAction("reconnect")}>{props.action === "reconnect" ? "Reconnecting…" : "Reconnect Agent"}</button>}
            <button className="danger-button" disabled={!canShutdown || props.action !== null} onClick={() => props.onAction("shutdown")}>{props.action === "shutdown" ? "Stopping…" : "Graceful stop"}</button>
          </div>
        </div>
        <div className="hero-meta">
          <Meta label="State" value={snapshot.state} />
          <Meta label="Last sample" value={new Date(snapshot.generated_at).toLocaleTimeString()} />
          <Meta label="Key fingerprint" value={snapshot.transport.key_fingerprint ? snapshot.transport.key_fingerprint.slice(0, 12) + "…" : "not configured"} mono />
        </div>
      </section>

      <section className="card-grid">
        <MetricCard label={snapshot.role === "gateway" ? "Connected Agents" : "Gateway session"} value={snapshot.role === "gateway" ? String(agents) : snapshot.agent?.connected ? "Connected" : "Offline"} detail={snapshot.role === "agent" ? "reconnects " + (snapshot.agent?.reconnects ?? 0) : "authenticated sessions"} tone={snapshot.ready ? "good" : "warn"} />
        <MetricCard label="Active streams" value={String(metrics.active_streams)} detail={metrics.connections + " transport connections"} />
        <MetricCard label="Throughput in" value={formatRate(inputRate)} detail={formatBytes(metrics.bytes_in_total) + " total"} />
        <MetricCard label="Throughput out" value={formatRate(outputRate)} detail={formatBytes(metrics.bytes_out_total) + " total"} />
        <MetricCard label={snapshot.role === "gateway" ? "Reverse mappings" : "Reverse registrations"} value={String(mappings)} detail={snapshot.role === "gateway" ? "public listeners" : "configured tunnels"} />
        <MetricCard label="QUIC RTT" value={formatDuration(metrics.quic.rtt_microseconds / 1000)} detail={metrics.quic.packets_lost + " packets lost"} tone={metrics.quic.packets_lost > 0 ? "warn" : "good"} />
      </section>

      <section className="chart-grid">
        <ChartCard title="Traffic rate" subtitle="Derived from cumulative counters">
          <div className="legend"><span><i className="legend-in" /> Inbound</span><span><i className="legend-out" /> Outbound</span></div>
          <Sparkline first={props.trend.input} second={props.trend.output} />
        </ChartCard>
        <ChartCard title="Error rate" subtitle="Auth, mapping and camouflage rejects">
          <div className="chart-kpi">{formatRate(errorRate)} <span>current</span></div>
          <Sparkline first={props.trend.errors} />
        </ChartCard>
      </section>

      {snapshot.role === "gateway" && snapshot.gateway && <GatewayTables gateway={snapshot.gateway} />}
      {snapshot.role === "agent" && snapshot.agent && <AgentTables agent={snapshot.agent} />}
      <EventPanel events={props.events} />
    </>
  );
}

function GatewayTables(props: { gateway: NonNullable<DashboardSnapshot["gateway"]> }) {
  return (
    <section className="split-grid">
      <TableCard title="Agents" count={props.gateway.agents.length}>
        <table><thead><tr><th>Agent</th><th>Session</th><th>Mappings</th><th>Status</th></tr></thead><tbody>
          {props.gateway.agents.map((agent) => <tr key={agent.agent_id}><td><strong>{agent.agent_id}</strong></td><td><code>{shortID(agent.session_id)}</code></td><td>{agent.mapping_count}</td><td><StatusPill good={agent.connected} text={agent.connected ? "Connected" : "Offline"} /></td></tr>)}
          {props.gateway.agents.length === 0 && <EmptyRow columns={4} text="No authenticated Agents" />}
        </tbody></table>
      </TableCard>
      <TableCard title="Reverse mappings" count={props.gateway.mappings.length}>
        <table><thead><tr><th>Name</th><th>Agent</th><th>Endpoint</th><th>State</th></tr></thead><tbody>
          {props.gateway.mappings.map((mapping) => <tr key={mapping.agent_id + "/" + mapping.name}><td><strong>{mapping.name}</strong></td><td>{mapping.agent_id}</td><td>{mapping.protocol.toUpperCase()} / {mapping.gateway_port}</td><td><StatusPill good={mapping.state === "active"} text={mapping.state} /></td></tr>)}
          {props.gateway.mappings.length === 0 && <EmptyRow columns={4} text="No reverse mappings registered" />}
        </tbody></table>
      </TableCard>
    </section>
  );
}

function AgentTables(props: { agent: NonNullable<DashboardSnapshot["agent"]> }) {
  return (
    <section className="split-grid">
      <TableCard title="Local proxy inbounds" count={props.agent.inbounds.length}>
        <table><thead><tr><th>Tag</th><th>Protocol</th><th>Listen</th></tr></thead><tbody>
          {props.agent.inbounds.map((inbound) => <tr key={inbound.tag}><td><strong>{inbound.tag}</strong></td><td>{inbound.protocol}</td><td><code>{inbound.listen}</code></td></tr>)}
          {props.agent.inbounds.length === 0 && <EmptyRow columns={3} text="No local proxy listeners" />}
        </tbody></table>
      </TableCard>
      <TableCard title="Reverse registrations" count={props.agent.reverse_mappings.length}>
        <table><thead><tr><th>Name</th><th>Protocol</th><th>Gateway port</th><th>Local</th></tr></thead><tbody>
          {props.agent.reverse_mappings.map((mapping) => <tr key={mapping.name}><td><strong>{mapping.name}</strong></td><td>{mapping.protocol}</td><td>{mapping.gateway_port}</td><td><code>{mapping.local}</code></td></tr>)}
          {props.agent.reverse_mappings.length === 0 && <EmptyRow columns={4} text="No reverse registrations" />}
        </tbody></table>
      </TableCard>
    </section>
  );
}

function EventPanel(props: { events: DashboardEvent[] }) {
  return (
    <section className="table-card event-card">
      <div className="section-heading"><div><p className="eyebrow">LIVE FEED</p><h3>Runtime events</h3></div><span className="count-badge">{props.events.length}</span></div>
      <div className="events">
        {props.events.length === 0 && <p className="empty">Waiting for structured runtime events…</p>}
        {props.events.map((event) => <div className="event-row" key={String(event.id) + event.event}><span className={"event-level " + event.level}>{event.level}</span><time>{new Date(event.time).toLocaleTimeString()}</time><strong>{event.event}</strong>{event.security_audit && <span className="audit-badge">AUDIT</span>}<span className="event-details">{Object.entries(event.attributes || {}).map(([key, value]) => key + "=" + value).join(" · ")}</span></div>)}
      </div>
    </section>
  );
}

function MetricCard(props: { label: string; value: string; detail: string; tone?: "good" | "warn" }) {
  return <article className="metric-card"><p className="eyebrow">{props.label}</p><strong className={props.tone || ""}>{props.value}</strong><span>{props.detail}</span></article>;
}

function ChartCard(props: { title: string; subtitle: string; children: React.ReactNode }) {
  return <article className="chart-card"><div className="section-heading"><div><h3>{props.title}</h3><p className="muted">{props.subtitle}</p></div></div>{props.children}</article>;
}

function TableCard(props: { title: string; count: number; children: React.ReactNode }) {
  return <article className="table-card"><div className="section-heading"><div><p className="eyebrow">RUNTIME INVENTORY</p><h3>{props.title}</h3></div><span className="count-badge">{props.count}</span></div>{props.children}</article>;
}

function Meta(props: { label: string; value: string; mono?: boolean }) {
  return <div className="meta-item"><span>{props.label}</span><strong className={props.mono ? "mono" : ""}>{props.value}</strong></div>;
}

function StatusPill(props: { good: boolean; text: string }) {
  return <span className={"status-pill " + (props.good ? "good" : "warn")}><i />{props.text}</span>;
}

function EmptyRow(props: { columns: number; text: string }) {
  return <tr><td colSpan={props.columns} className="empty">{props.text}</td></tr>;
}

function Sparkline(props: { first: MetricPoint[]; second?: MetricPoint[] }) {
  const width = 640;
  const height = 170;
  const all = [...props.first, ...(props.second || [])];
  const max = Math.max(1, ...all.map((point) => point.value));
  const line = (points: MetricPoint[]) => points.map((point, index) => {
    const x = points.length <= 1 ? 0 : (index / (points.length - 1)) * width;
    const y = height - (point.value / max) * (height - 12) - 6;
    return x.toFixed(1) + "," + y.toFixed(1);
  }).join(" ");
  return <svg className="sparkline" viewBox={"0 0 " + width + " " + height} role="img" aria-label="metric trend"><path className="grid-line" d={"M0 " + height * 0.25 + "H" + width + " M0 " + height * 0.5 + "H" + width + " M0 " + height * 0.75 + "H" + width} />{props.second && <polyline className="line-out" points={line(props.second)} />}{<polyline className="line-in" points={line(props.first)} />}</svg>;
}

function latest(points: MetricPoint[]): number {
  return points.length === 0 ? 0 : points[points.length - 1].value;
}

function shortID(value: string): string {
  if (value.length <= 14) return value;
  return value.slice(0, 6) + "…" + value.slice(-5);
}
