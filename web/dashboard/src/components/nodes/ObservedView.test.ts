import { afterEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";
import ObservedView from "./ObservedView.vue";

const apiMocks = vi.hoisted(() => ({
  getObserved: vi.fn(),
  getNodeRuntimeConnections: vi.fn(),
  getRuntimeSettings: vi.fn(),
  listRuntimeEvents: vi.fn(),
  listRuntimeTraffic: vi.fn(),
}));

vi.mock("../../controller-api", async () => ({
  ...(await vi.importActual<typeof import("../../controller-api")>("../../controller-api")),
  getObserved: apiMocks.getObserved,
  getNodeRuntimeConnections: apiMocks.getNodeRuntimeConnections,
  getRuntimeSettings: apiMocks.getRuntimeSettings,
  listRuntimeEvents: apiMocks.listRuntimeEvents,
  listRuntimeTraffic: apiMocks.listRuntimeTraffic,
}));

class EventSourceStub {
  addEventListener() {}
  close() {}
}

const stubs = {
  DataTable: { template: "<table><slot /></table>" },
  EmptyState: { template: "<div><slot /></div>" },
  Spinner: { template: "<span />" },
  StatusPill: { props: ["tone"], template: '<span class="status-pill"><slot /></span>' },
};

describe("ObservedView", () => {
  afterEach(() => {
    apiMocks.getObserved.mockReset();
    apiMocks.getNodeRuntimeConnections.mockReset();
    apiMocks.getRuntimeSettings.mockReset();
    apiMocks.listRuntimeEvents.mockReset();
    apiMocks.listRuntimeTraffic.mockReset();
    vi.unstubAllGlobals();
  });

  it("surfaces whether the latest apply error is retryable", async () => {
    vi.stubGlobal("EventSource", EventSourceStub);
    apiMocks.getObserved.mockResolvedValue({
      schema_version: 10,
      node_id: "node-1",
      applied_generation: 2,
      healthy: false,
      degraded: true,
      last_error: { code: "port_conflict", message: "port is already in use", retryable: true },
      observed_at: "2026-09-03T00:00:00Z",
    });
    apiMocks.getNodeRuntimeConnections.mockResolvedValue({ items: [] });
    apiMocks.getRuntimeSettings.mockResolvedValue({ advanced_operations_enabled: false, runtime_retention_days: 30 });
    apiMocks.listRuntimeEvents.mockResolvedValue({ items: [] });
    apiMocks.listRuntimeTraffic.mockResolvedValue({ items: [] });

    const wrapper = mount(ObservedView, { props: { nodeId: "node-1" }, global: { stubs } });
    await flushPromises();

    expect(wrapper.find(".last-error").text()).toContain("可重试");
    wrapper.unmount();
  });
});
