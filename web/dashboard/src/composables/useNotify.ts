import { reactive } from "vue";

export type ToastTone = "success" | "error";

export interface ToastItem {
  id: number;
  tone: ToastTone;
  message: string;
}

// Module singleton: every page publishes toasts rendered by AppShell's
// ToastHost.
const toasts = reactive<ToastItem[]>([]);
let nextId = 1;

export function useNotify() {
  function dismiss(id: number) {
    const index = toasts.findIndex((toast) => toast.id === id);
    if (index >= 0) toasts.splice(index, 1);
  }

  function push(tone: ToastTone, message: string) {
    const id = nextId++;
    toasts.push({ id, tone, message });
    window.setTimeout(() => dismiss(id), 4000);
  }

  return {
    toasts,
    dismiss,
    success: (message: string) => push("success", message),
    error: (message: string) => push("error", message),
  };
}
