import { inject, type InjectionKey, type Ref } from "vue";
import type { DashboardEvent, DashboardSnapshot } from "./api";
import type { TrendState } from "./dashboard";

export interface DashboardContext {
  snapshot: Ref<DashboardSnapshot | null>;
  events: Ref<DashboardEvent[]>;
  trend: Ref<TrendState>;
  error: Ref<string>;
  streamState: Ref<string>;
}

export const dashboardKey: InjectionKey<DashboardContext> = Symbol("asterferry-dashboard");

export function useDashboardContext(): DashboardContext {
  const context = inject(dashboardKey);
  if (!context) throw new Error("dashboard context is unavailable");
  return context;
}
