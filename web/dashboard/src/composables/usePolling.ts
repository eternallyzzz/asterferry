import { onMounted, onUnmounted } from "vue";

// Page-scoped polling runs once on mount, then at the configured interval, and
// is cleaned up on unmount. The fetcher handles its own errors, usually by
// publishing a toast, so one failure does not stop the loop.
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
