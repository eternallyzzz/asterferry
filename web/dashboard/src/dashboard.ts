import { ref, watch, type Ref } from "vue";
import {
  APIError,
  consumeEventStream,
  fetchSnapshot,
  type DashboardEvent,
  type DashboardSnapshot,
} from "./api";
import { viewerTokenErrorMessage } from "./session";
import {
  appendPoint,
  counterRate,
  snapshotErrorTotal,
  type MetricPoint,
} from "./model";

export interface TrendState {
  input: MetricPoint[];
  output: MetricPoint[];
  errors: MetricPoint[];
}

const emptyTrend = (): TrendState => ({ input: [], output: [], errors: [] });

export function useDashboard(viewerToken: Ref<string>, onUnauthorized: (message: string) => void) {
  const snapshot = ref<DashboardSnapshot | null>(null);
  const events = ref<DashboardEvent[]>([]);
  const trend = ref<TrendState>(emptyTrend());
  const error = ref("");
  const streamState = ref("offline");
  const lastEventID = ref(0);
  let previous: { snapshot: DashboardSnapshot; time: number } | null = null;

  watch(viewerToken, (token, _oldToken, onCleanup) => {
    let active = true;
    const controller = new AbortController();
    previous = null;
    snapshot.value = null;
    events.value = [];
    trend.value = emptyTrend();
    error.value = "";
    streamState.value = token ? "connecting" : "offline";
    lastEventID.value = 0;

    if (!token) {
      onCleanup(() => controller.abort());
      return;
    }

    const applySnapshot = (next: DashboardSnapshot) => {
      const now = Date.now();
      if (previous) {
        const elapsed = now - previous.time;
        const oldMetrics = previous.snapshot.metrics;
        trend.value = {
          input: appendPoint(trend.value.input, { time: now, value: counterRate(next.metrics.bytes_in_total, oldMetrics.bytes_in_total, elapsed) }),
          output: appendPoint(trend.value.output, { time: now, value: counterRate(next.metrics.bytes_out_total, oldMetrics.bytes_out_total, elapsed) }),
          errors: appendPoint(trend.value.errors, { time: now, value: counterRate(snapshotErrorTotal(next.metrics), snapshotErrorTotal(oldMetrics), elapsed) }),
        };
      }
      previous = { snapshot: next, time: now };
      snapshot.value = next;
      error.value = "";
    };

    const refresh = async () => {
      try {
        applySnapshot(await fetchSnapshot(token));
      } catch (caught) {
        if (!active) return;
        if (caught instanceof APIError && caught.status === 401) {
          error.value = viewerTokenErrorMessage;
          onUnauthorized(viewerTokenErrorMessage);
          return;
        }
        error.value = caught instanceof Error ? caught.message : "Dashboard 刷新失败。";
      }
    };

    const wait = (ms: number) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));
    const stream = async () => {
      while (active && !controller.signal.aborted) {
        try {
          streamState.value = "connecting";
          await consumeEventStream(token, lastEventID.value, {
            onOpen: () => { streamState.value = "connected"; },
            onEvent: (event) => {
              lastEventID.value = Math.max(lastEventID.value, event.id);
              events.value = [event, ...events.value].slice(0, 80);
            },
            onGap: (from, to) => {
              events.value = [{ id: to, time: new Date().toISOString(), level: "warn", event: "events.gap", attributes: { from: String(from), to: String(to) } }, ...events.value].slice(0, 80);
            },
          }, controller.signal);
          if (active) streamState.value = "reconnecting";
        } catch (caught) {
          if (!active || controller.signal.aborted) return;
          if (caught instanceof APIError && caught.status === 401) {
            error.value = viewerTokenErrorMessage;
            onUnauthorized(viewerTokenErrorMessage);
            return;
          }
          streamState.value = "reconnecting";
        }
        await wait(2000);
      }
    };

    void refresh();
    const refreshTimer = window.setInterval(() => void refresh(), 5000);
    void stream();
    onCleanup(() => {
      active = false;
      controller.abort();
      window.clearInterval(refreshTimer);
    });
  }, { immediate: true });

  return { snapshot, events, trend, error, streamState };
}
