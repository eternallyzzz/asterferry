import { useEffect, useMemo, useState } from "react";
import {
  fetchConfig,
  type ConfigObject,
  type ConfigPayload,
  type ConfigSnapshot,
  type ConfigValidation,
  validateConfig,
} from "./api";

type EditorMode = "form" | "yaml";

export default function ConfigurationView(props: { token: string; onClose: () => void }) {
  const [snapshot, setSnapshot] = useState<ConfigSnapshot | null>(null);
  const [values, setValues] = useState<ConfigObject | null>(null);
  const [yaml, setYaml] = useState("");
  const [mode, setMode] = useState<EditorMode>("form");
  const [validation, setValidation] = useState<ConfigValidation | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");

  const load = async () => {
    setBusy(true);
    setError("");
    try {
      const next = await fetchConfig(props.token);
      setSnapshot(next);
      setValues(cloneObject(next.values));
      setYaml(next.yaml);
      setValidation(null);
      setMessage("");
      setMode("form");
    } catch (caught) {
      setError(caught instanceof Error ? caught.message : "Configuration load failed.");
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => { void load(); }, [props.token]);

  const payload = useMemo<ConfigPayload | null>(() => {
    if (!snapshot || !values) return null;
    return mode === "yaml"
      ? { base_revision: snapshot.revision, yaml }
      : { base_revision: snapshot.revision, config: values };
  }, [mode, snapshot, values, yaml]);

  const validateCurrent = async (): Promise<ConfigValidation | null> => {
    if (!payload) return null;
    setBusy(true);
    setError("");
    setMessage("");
    try {
      const result = await validateConfig(props.token, payload);
      setValidation(result);
      setMessage(result.changed ? "Configuration is valid. Review the diff before applying." : "No configuration changes detected.");
      return result;
    } catch (caught) {
      setValidation(null);
      setError(caught instanceof Error ? caught.message : "Configuration validation failed.");
      return null;
    } finally {
      setBusy(false);
    }
  };

  const switchMode = (next: EditorMode) => {
    if (next === mode) return;
    if (mode === "yaml" && next === "form" && snapshot && yaml !== snapshot.yaml && !window.confirm("Switching to the structured form will discard unsaved YAML edits. Continue?")) {
      return;
    }
    if (next === "yaml" && values) setYaml(renderYAML(values));
    setMode(next);
    setValidation(null);
    setMessage("");
  };

  return (
    <section className="configuration-page">
      <div className="section-heading config-heading">
        <div>
          <p className="eyebrow">CONFIGURATION / {snapshot?.role || "LOADING"}</p>
          <h2>Inspect runtime configuration</h2>
          <p className="muted">Drafts are validated and previewed here only. Apply changes with the Admin token through the CLI or protected API.</p>
        </div>
        <div className="topbar-actions">
          <button className="quiet-button" onClick={props.onClose}>Back to dashboard</button>
          <button className="quiet-button" onClick={() => void load()} disabled={busy}>Reload</button>
        </div>
      </div>

      {error && <div className="notice error">{error}</div>}
      {message && <div className="notice">{message}</div>}
      {snapshot && values ? (
        <>
          <div className="config-meta">
            <span className="status-pill warn"><i />Viewer mode</span>
            <span>{snapshot.writable ? "File writable by CLI/API" : "Read-only configuration mount"}</span>
            <span>{snapshot.backup_available ? "Previous backup available" : "No backup yet"}</span>
            <code>{snapshot.revision.slice(0, 16)}…</code>
          </div>
          <div className="config-tabs">
            <button className={mode === "form" ? "active" : ""} onClick={() => switchMode("form")}>Structured form</button>
            <button className={mode === "yaml" ? "active" : ""} onClick={() => switchMode("yaml")}>Advanced YAML</button>
          </div>
          {mode === "form" ? <ConfigForm values={values} onChange={setValues} role={snapshot.role} /> : (
            <label className="config-editor-label">Full configuration YAML
              <textarea className="config-editor" value={yaml} onChange={(event) => { setYaml(event.target.value); setValidation(null); }} spellCheck={false} />
            </label>
          )}
          {validation && (
            <div className="config-validation">
              <strong>{validation.changed ? "Valid changes" : "No changes"}</strong>
              {validation.warnings?.map((warning) => <p className="warning-line" key={warning}>{warning}</p>)}
              {validation.diff && <pre>{validation.diff}</pre>}
            </div>
          )}
          <div className="config-actions">
            <button className="secondary-button" onClick={() => void validateCurrent()} disabled={busy}>Validate and preview</button>
          </div>
          <p className="muted config-hint">The Dashboard never writes configuration. Use <code>asterferry config apply --config … --file … --yes</code> or an Admin-scoped API client.</p>
        </>
      ) : !error && <div className="loading-card"><span className="spinner" /> Loading configuration…</div>}
    </section>
  );
}

function ConfigForm(props: { values: ConfigObject; onChange: (values: ConfigObject) => void; role: "gateway" | "agent" }) {
  const read = (path: string, fallback: string | number | boolean): string | number | boolean => {
    const value = getPath(props.values, path);
    return typeof value === "string" || typeof value === "number" || typeof value === "boolean" ? value : fallback;
  };
  const update = (path: string, value: string | number | boolean) => props.onChange(setPath(props.values, path, value));
  return (
    <div className="config-form-grid">
      <ConfigCard title="Management web">
        <TextField label="Management listen" value={String(read("management.listen", "127.0.0.1:9090"))} onChange={(value) => update("management.listen", value)} />
        <CheckboxField label="Embedded Dashboard enabled" checked={Boolean(read("management.web.enabled", true))} onChange={(value) => update("management.web.enabled", value)} />
        <TextField label="TLS certificate file" value={String(read("management.tls.cert_file", ""))} onChange={(value) => update("management.tls.cert_file", value)} />
        <TextField label="TLS key file" value={String(read("management.tls.key_file", ""))} onChange={(value) => update("management.tls.key_file", value)} />
        <TextField label="TLS CA file (optional CLI trust)" value={String(read("management.tls.ca_file", ""))} onChange={(value) => update("management.tls.ca_file", value)} />
      </ConfigCard>
      <ConfigCard title="Runtime">
        <TextField label="Logging level" value={String(read("logging.level", "info"))} onChange={(value) => update("logging.level", value)} />
        <TextField label="Logging format" value={String(read("logging.format", "json"))} onChange={(value) => update("logging.format", value)} />
        <NumberField label="Shutdown grace period (seconds)" value={Number(read("shutdown.grace_period_seconds", 30))} onChange={(value) => update("shutdown.grace_period_seconds", value)} />
        <TextField label="Transport ALPN" value={String(read("transport.alpn", ""))} onChange={(value) => update("transport.alpn", value)} />
        <NumberField label="Handshake timeout (seconds)" value={Number(read("transport.handshake_timeout_seconds", 10))} onChange={(value) => update("transport.handshake_timeout_seconds", value)} />
      </ConfigCard>
      <ConfigCard title={props.role === "gateway" ? "Gateway" : "Agent"}>
        {props.role === "gateway" ? (
          <TextField label="Gateway listen" value={String(read("gateway.listen", ""))} onChange={(value) => update("gateway.listen", value)} />
        ) : (
          <>
            <TextField label="Gateway server" value={String(read("agent.server", ""))} onChange={(value) => update("agent.server", value)} />
            <TextField label="Default route" value={String(read("agent.proxy.default_route", "gateway"))} onChange={(value) => update("agent.proxy.default_route", value)} />
          </>
        )}
        <p className="muted config-hint">Use Advanced YAML for agent lists, reverse mappings, ACLs, routing rules, and other complete role settings.</p>
      </ConfigCard>
    </div>
  );
}

function ConfigCard(props: { title: string; children: React.ReactNode }) {
  return <section className="table-card config-card"><div className="section-heading"><h3>{props.title}</h3></div>{props.children}</section>;
}

function TextField(props: { label: string; value: string; onChange: (value: string) => void }) {
  return <label className="config-field"><span>{props.label}</span><input value={props.value} onChange={(event) => props.onChange(event.target.value)} /></label>;
}

function NumberField(props: { label: string; value: number; onChange: (value: number) => void }) {
  return <label className="config-field"><span>{props.label}</span><input type="number" value={props.value} onChange={(event) => props.onChange(Number(event.target.value))} /></label>;
}

function CheckboxField(props: { label: string; checked: boolean; onChange: (value: boolean) => void }) {
  return <label className="config-checkbox"><input type="checkbox" checked={props.checked} onChange={(event) => props.onChange(event.target.checked)} /><span>{props.label}</span></label>;
}

function cloneObject(value: ConfigObject): ConfigObject {
  return JSON.parse(JSON.stringify(value)) as ConfigObject;
}

function getPath(root: ConfigObject, path: string): import("./api").ConfigValue | undefined {
  let value: import("./api").ConfigValue = root;
  for (const key of path.split(".")) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
    value = (value as ConfigObject)[key];
  }
  return value;
}

function setPath(root: ConfigObject, path: string, value: string | number | boolean): ConfigObject {
  const next = cloneObject(root);
  const keys = path.split(".");
  let cursor = next;
  for (const key of keys.slice(0, -1)) {
    const child = cursor[key];
    if (!child || typeof child !== "object" || Array.isArray(child)) cursor[key] = {};
    cursor = cursor[key] as ConfigObject;
  }
  cursor[keys[keys.length - 1]] = value;
  return next;
}

function renderYAML(value: import("./api").ConfigValue, indent = 0): string {
  const pad = " ".repeat(indent);
  if (value === null || typeof value !== "object") return scalarYAML(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value.map((item) => {
      if (item !== null && typeof item === "object") return `${pad}-\n${renderYAML(item, indent + 2)}`;
      return `${pad}- ${scalarYAML(item)}`;
    }).join("\n");
  }
  const lines: string[] = [];
  for (const [key, child] of Object.entries(value)) {
    if (child !== null && typeof child === "object") {
      const rendered = renderYAML(child, indent + 2);
      lines.push(`${pad}${key}:`);
      lines.push(rendered);
    } else {
      lines.push(`${pad}${key}: ${scalarYAML(child)}`);
    }
  }
  return lines.join("\n");
}

function scalarYAML(value: string | number | boolean | null): string {
  if (value === null) return "null";
  if (typeof value === "string") return JSON.stringify(value);
  return String(value);
}
