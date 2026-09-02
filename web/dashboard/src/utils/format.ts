// 从原 ControllerShell 提取的纯展示/解析工具，供各页面复用。
import { ControllerAPIError } from "../controller-api";

export function formatTime(value: string | undefined | null): string {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString("zh-CN", { hour12: false });
}

export function userRoleLabel(role: string): string {
  return ({ viewer: "Viewer · 只读", operator: "Operator · 运维", admin: "Admin · 管理员" } as Record<string, string>)[role] || role;
}

export function stateLabel(state: string): string {
  return ({ applied: "已应用", pending: "等待", degraded: "降级", draining: "排空" } as Record<string, string>)[state] || state || "未知";
}

export function certificateTone(state: string): "good" | "warn" | "bad" {
  return state === "active" ? "good" : state === "revoked" || state === "expired" ? "bad" : "warn";
}

export function describeError(caught: unknown): string {
  if (caught instanceof ControllerAPIError) {
    if (caught.code === "revision_conflict") return "资源已被其他操作者修改，请刷新后重试（revision 冲突）。";
    if (caught.code === "already_exists") return "资源已存在，请使用其他 ID。";
    if (caught.code === "bootstrap_unavailable" && caught.message.includes("grpc_advertise")) {
      return "Controller 尚未配置 Node 可访问的 gRPC 地址（grpc_advertise）。请先执行 controller configure --grpc-advertise <host:port> 并重启 Controller。";
    }
    if (caught.status === 409) return caught.message || "请求冲突，请刷新后重试。";
    if (caught.status === 403) return "当前账户没有执行此操作的权限。";
    if (caught.status === 404) return "资源不存在或已被删除。";
    return caught.message;
  }
  return caught instanceof Error ? caught.message : "Controller 请求失败。";
}

export function prettyJson(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return "—";
  }
}

export function parseObject(value: string, field: string): Record<string, unknown> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value || "{}");
  } catch {
    throw new Error(`${field} 必须是有效 JSON。`);
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(`${field} 必须是 JSON 对象。`);
  return parsed as Record<string, unknown>;
}

export function parseLabels(value: string): Record<string, string> {
  const parsed = parseObject(value, "labels");
  const labels: Record<string, string> = {};
  for (const [name, item] of Object.entries(parsed)) {
    if (typeof item !== "string") throw new Error("labels 的值必须是字符串。");
    labels[name] = item;
  }
  return labels;
}

// 逗号/换行分隔的文本与字符串数组互转（egress 端口段、CIDR、route domains 等）。
export function joinList(value: string[] | undefined | null): string {
  return (value ?? []).join(", ");
}

export function splitList(value: string): string[] {
  return value
    .split(/[\n,]+/)
    .map((item) => item.trim())
    .filter(Boolean);
}

export function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

export async function copyText(value: string): Promise<boolean> {
  if (!value || typeof navigator === "undefined" || !navigator.clipboard) return false;
  try {
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}
