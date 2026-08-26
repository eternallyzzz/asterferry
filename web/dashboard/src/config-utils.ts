import type { ConfigObject, ConfigValue } from "./api";

export function cloneObject(value: ConfigObject): ConfigObject {
  return JSON.parse(JSON.stringify(value)) as ConfigObject;
}

export function getPath(root: ConfigObject, path: string): ConfigValue | undefined {
  let value: ConfigValue = root;
  for (const key of path.split(".")) {
    if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
    value = (value as ConfigObject)[key];
  }
  return value;
}

export function setPath(root: ConfigObject, path: string, value: string | number | boolean): ConfigObject {
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

export function renderYAML(value: ConfigValue, indent = 0): string {
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
      lines.push(`${pad}${key}:`);
      lines.push(renderYAML(child, indent + 2));
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
