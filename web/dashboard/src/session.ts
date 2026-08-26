import { ref } from "vue";

const viewerToken = ref("");
const adminToken = ref("");
const viewerError = ref("");

export const viewerTokenErrorMessage = "Viewer token 已失效，请重新解锁。";

export function useSession() {
  const unlock = (token: string) => {
    viewerToken.value = token.trim();
    adminToken.value = "";
    viewerError.value = "";
  };

  const setAdminToken = (token: string) => {
    adminToken.value = token.trim();
  };

  const lock = () => {
    viewerToken.value = "";
    adminToken.value = "";
    viewerError.value = "";
  };

  const invalidateViewer = (message = viewerTokenErrorMessage) => {
    viewerToken.value = "";
    adminToken.value = "";
    viewerError.value = message;
  };

  const clearAdmin = () => {
    adminToken.value = "";
  };

  return { viewerToken, adminToken, viewerError, unlock, setAdminToken, clearAdmin, lock, invalidateViewer };
}
