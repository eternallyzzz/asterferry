import { afterEach, describe, expect, it, vi } from "vitest";
import { defineComponent } from "vue";
import { mount } from "@vue/test-utils";
import { usePolling } from "./usePolling";

async function flushMicrotasks() {
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
}

describe("usePolling", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("coalesces overlapping refreshes and runs one follow-up", async () => {
    vi.useFakeTimers();
    let resolveFirst!: () => void;
    const fetcher = vi.fn().mockImplementation(() => {
      if (fetcher.mock.calls.length === 1) {
        return new Promise<void>((resolve) => {
          resolveFirst = resolve;
        });
      }
      return Promise.resolve();
    });
    let refresh!: () => Promise<void>;
    const TestComponent = defineComponent({
      setup() {
        ({ refresh } = usePolling(fetcher, 1_000));
        return {};
      },
      template: "<div />",
    });
    const wrapper = mount(TestComponent);

    expect(fetcher).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1_000);
    const queuedRefresh = refresh();
    expect(fetcher).toHaveBeenCalledTimes(1);

    resolveFirst();
    await queuedRefresh;
    await flushMicrotasks();
    expect(fetcher).toHaveBeenCalledTimes(2);

    wrapper.unmount();
  });

  it("does not start a queued refresh after unmount", async () => {
    let resolveFirst!: () => void;
    const fetcher = vi.fn().mockImplementation(() => new Promise<void>((resolve) => {
      resolveFirst = resolve;
    }));
    let refresh!: () => Promise<void>;
    const TestComponent = defineComponent({
      setup() {
        ({ refresh } = usePolling(fetcher));
        return {};
      },
      template: "<div />",
    });
    const wrapper = mount(TestComponent);

    void refresh();
    wrapper.unmount();
    resolveFirst();
    await flushMicrotasks();

    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
