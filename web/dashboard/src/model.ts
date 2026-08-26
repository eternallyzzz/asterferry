import type { MetricsSnapshot } from "./api";

export interface MetricPoint {
  time: number;
  value: number;
}

export function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return "—";
  if (value < 1024) return Math.round(value) + " B";
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let scaled = value;
  let unit = -1;
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024;
    unit += 1;
  }
  return scaled.toFixed(scaled >= 10 ? 1 : 2) + " " + units[unit];
}

export function formatRate(bytesPerSecond: number): string {
  return formatBytes(bytesPerSecond) + "/s";
}

export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return Math.round(ms) + " ms";
  return (ms / 1000).toFixed(ms < 10000 ? 1 : 0) + " s";
}

export function counterRate(current: number, previous: number, elapsedMs: number): number {
  if (!Number.isFinite(current) || !Number.isFinite(previous) || elapsedMs <= 0 || current < previous) return 0;
  return ((current - previous) * 1000) / elapsedMs;
}

export function appendPoint(points: MetricPoint[], point: MetricPoint, limit = 60): MetricPoint[] {
  const next = [...points, point];
  return next.length > limit ? next.slice(next.length - limit) : next;
}

export function snapshotErrorTotal(metrics: MetricsSnapshot): number {
  return metrics.auth_failures_total + metrics.management_auth_failures_total + metrics.management_auth_rate_limited_total + metrics.mapping_failures_total + metrics.obfuscation_packets_rejected_total;
}

export function statusLabel(snapshot: { ready: boolean; state: string }): string {
  if (snapshot.state === "draining") return "正在排空";
  if (snapshot.state === "stopped") return "已停止";
  return snapshot.ready ? "运行正常" : "状态降级";
}

export function stateTone(snapshot: { ready: boolean; state: string }): "green" | "orange" | "red" {
  if (snapshot.state === "stopped") return "red";
  if (snapshot.state === "draining" || !snapshot.ready) return "orange";
  return "green";
}

export function mappingStateLabel(state: string): string {
  switch (state) {
    case "active": return "在线";
    case "pending": return "等待连接";
    case "closed": return "已关闭";
    case "failed": return "失败";
    default: return state || "未知";
  }
}

export function formatTime(value: string | number | Date): string {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("zh-CN", { hour12: false });
}
