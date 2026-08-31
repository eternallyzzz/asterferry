import { onMounted, onUnmounted } from "vue";

// 页面级轮询：挂载时立即执行一次，之后按间隔执行；卸载时清理。
// fetcher 内部自行处理错误（通常降级为 Toast），轮询循环不中断。
export function usePolling(fetcher: () => Promise<void> | void, intervalMs = 10_000) {
  let timer: number | undefined;

  async function refresh() {
    await fetcher();
  }

  onMounted(() => {
    void refresh();
    timer = window.setInterval(() => void refresh(), intervalMs);
  });

  onUnmounted(() => {
    if (timer !== undefined) window.clearInterval(timer);
  });

  return { refresh };
}
