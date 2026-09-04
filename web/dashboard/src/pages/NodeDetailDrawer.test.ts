import { afterEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount, type VueWrapper } from "@vue/test-utils";
import type { ComponentPublicInstance } from "vue";
import type { ControllerNode } from "../controller-api";
import NodeDetailDrawer from "./NodeDetailDrawer.vue";

const apiMocks = vi.hoisted(() => ({
  getNodeSpec: vi.fn(),
}));

vi.mock("../controller-api", async () => ({
  ...(await vi.importActual<typeof import("../controller-api")>("../controller-api")),
  getNodeSpec: apiMocks.getNodeSpec,
}));

const stubs = {
  DrawerPanel: {
    props: ["open", "title"],
    template: '<div v-if="open" class="drawer-stub"><slot /></div>',
  },
  PanelCard: {
    props: ["title"],
    template: '<section class="panel-stub"><h2 v-if="title">{{ title }}</h2><slot name="actions" /><slot /></section>',
  },
  StatusPill: { template: "<span><slot /></span>" },
  JsonEditor: {
    props: ["modelValue"],
    template: '<textarea class="json-editor-stub" :value="modelValue" />',
  },
  ModalDialog: {
    props: ["open"],
    template: '<div v-if="open"><slot /><slot name="footer" /></div>',
  },
  Spinner: { template: "<span class='spinner-stub' />" },
  EgressForm: { template: "<div class='egress-stub' />" },
  ProxyRouteManager: { template: "<div class='proxy-stub' />" },
  ObservedView: { template: "<div class='observed-stub' />" },
  SnapshotView: { template: "<div class='snapshot-stub' />" },
};

function makeNode(id: string): ControllerNode {
  return {
    id,
    spec_kind: "gateway",
    name: id,
    labels: {},
    enabled: true,
    certificate_state: "active",
    certificate_serial: `serial-${id}`,
    revision: 1,
    created_at: "2026-08-31T00:00:00Z",
    updated_at: "2026-08-31T00:00:00Z",
  };
}

function makeSpec(id: string) {
  return { node_id: id, revision: 1, listeners: [] };
}

function buttonFor(wrapper: VueWrapper<ComponentPublicInstance>, label: string) {
  const button = wrapper.findAll("button").find((candidate) => candidate.text() === label);
  if (!button) throw new Error(`section button not found: ${label}`);
  return button;
}

function mountDrawer(node: ControllerNode, open = true) {
  return mount(NodeDetailDrawer, {
    props: { open, node },
    global: { stubs },
  });
}

describe("NodeDetailDrawer", () => {
  afterEach(() => {
    apiMocks.getNodeSpec.mockReset();
  });

  it("keeps the selected section when the same node object is refreshed", async () => {
    const node = makeNode("gw-1");
    apiMocks.getNodeSpec.mockResolvedValue({ node_id: node.id, kind: "gateway", gateway: makeSpec(node.id), revision: 1 });
    const wrapper = mountDrawer(node);
    await flushPromises();

    await buttonFor(wrapper, "观测").trigger("click");
    expect(buttonFor(wrapper, "观测").classes()).toContain("active");

    await wrapper.setProps({
      node: { ...node, updated_at: "2026-08-31T00:00:10Z" },
    });

    expect(buttonFor(wrapper, "观测").classes()).toContain("active");
    wrapper.unmount();
  });

  it("shows the certificate serial as read-only node metadata", async () => {
    const node = makeNode("gw-serial");
    apiMocks.getNodeSpec.mockResolvedValue({ node_id: node.id, kind: "gateway", gateway: makeSpec(node.id), revision: 1 });
    const wrapper = mountDrawer(node);
    await flushPromises();

    expect(wrapper.text()).toContain("serial-gw-serial");
    wrapper.unmount();
  });

  it("resets the section when switching nodes or reopening the drawer", async () => {
    const first = makeNode("gw-1");
    const second = makeNode("gw-2");
    apiMocks.getNodeSpec.mockImplementation(async (id: string) => ({ node_id: id, kind: "gateway", gateway: makeSpec(id), revision: 1 }));
    const wrapper = mountDrawer(first);
    await flushPromises();

    await buttonFor(wrapper, "快照").trigger("click");
    expect(buttonFor(wrapper, "快照").classes()).toContain("active");

    await wrapper.setProps({ node: second });
    await flushPromises();
    expect(buttonFor(wrapper, "信息").classes()).toContain("active");

    await buttonFor(wrapper, "快照").trigger("click");
    await wrapper.setProps({ open: false });
    await wrapper.setProps({ open: true });
    await flushPromises();
    expect(buttonFor(wrapper, "信息").classes()).toContain("active");
    wrapper.unmount();
  });

  it("ignores a stale specification response from the previous node", async () => {
    const first = makeNode("gw-1");
    const second = makeNode("gw-2");
    const pending = new Map<string, (value: unknown) => void>();
    apiMocks.getNodeSpec.mockImplementation((id: string) => new Promise((resolve) => {
      pending.set(id, resolve);
    }));
    const wrapper = mountDrawer(first);
    await wrapper.setProps({ node: second });

    pending.get(first.id)?.({ node_id: first.id, kind: "gateway", gateway: makeSpec(first.id), revision: 1 });
    await flushPromises();
    expect(wrapper.find(".json-editor-stub").exists()).toBe(false);

    pending.get(second.id)?.({ node_id: second.id, kind: "gateway", gateway: makeSpec(second.id), revision: 1 });
    await flushPromises();
    expect((wrapper.get(".json-editor-stub").element as HTMLTextAreaElement).value).toContain(second.id);
    wrapper.unmount();
  });
});
