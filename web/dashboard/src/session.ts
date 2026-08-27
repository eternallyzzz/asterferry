import { ref } from "vue";
import { currentUser, login as controllerLogin, logout as controllerLogout, type ControllerUser } from "./controller-api";

const viewerToken = ref("");
const adminToken = ref("");
const viewerError = ref("");
const controllerUser = ref<ControllerUser | null>(null);
const controllerError = ref("");

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
    controllerUser.value = null;
    controllerError.value = "";
  };

  const invalidateViewer = (message = viewerTokenErrorMessage) => {
    viewerToken.value = "";
    adminToken.value = "";
    viewerError.value = message;
  };

  const clearAdmin = () => {
    adminToken.value = "";
  };

  const login = async (username: string, password: string) => {
    try {
      const result = await controllerLogin(username, password);
      controllerUser.value = result.user;
      controllerError.value = "";
      return result.user;
    } catch (error) {
      controllerError.value = error instanceof Error ? error.message : "Controller 登录失败。";
      throw error;
    }
  };

  const logout = async () => {
    try {
      await controllerLogout();
    } finally {
      controllerUser.value = null;
    }
  };

  // Cookie sessions survive a page reload. Restore them opportunistically;
  // an unauthenticated response is a normal state for the login screen and
  // must not surface as a noisy error to the user.
  const restore = async () => {
    try {
      const user = await currentUser();
      controllerUser.value = user;
      controllerError.value = "";
      return user;
    } catch {
      controllerUser.value = null;
      return null;
    }
  };

  return { viewerToken, adminToken, viewerError, controllerUser, controllerError, unlock, setAdminToken, clearAdmin, lock, invalidateViewer, login, logout, restore };
}
