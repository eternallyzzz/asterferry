import { onMounted, onUnmounted } from "vue";

// 页面级轮询：挂载时立即执行一次，之后按间隔执行；卸载时清理。
// fetcher 内部自行处理错误（通常降级为 Toast），轮询循环不中断。
export function usePolling(fetcher: () => Promise<void> | void, intervalMs = 10_000) {
  let timer: number | undefined;
  let inFlight: Promise<void> | undefined;
  let rerunRequested = false;
  let disposed = false;

  async function refresh() {
    if (inFlight) {
      rerunRequested = true;
      await inFlight;
      return;
    }

    const current = (async () => {
      do {
        rerunRequested = false;
        await fetcher();
      } while (rerunRequested && !disposed);
    })();
    inFlight = current;
    try {
      await current;
    } finally {
      if (inFlight === current) inFlight = undefined;
    }
  }

  onMounted(() => {
    void refresh();
    timer = window.setInterval(() => void refresh(), intervalMs);
  });

  onUnmounted(() => {
    disposed = true;
    if (timer !== undefined) window.clearInterval(timer);
  });

  return { refresh };
}
