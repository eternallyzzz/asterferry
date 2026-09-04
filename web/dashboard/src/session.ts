import { computed, ref } from "vue";
import { currentUser, login as controllerLogin, logout as controllerLogout, type ControllerUser } from "./controller-api";

const controllerUser = ref<ControllerUser | null>(null);
const controllerError = ref("");

// 权限点自原 ControllerShell 平移：viewer 只读、operator 管业务资源、admin 管身份。
const canOperate = computed(() => controllerUser.value?.role === "operator" || controllerUser.value?.role === "admin");
const canAdmin = computed(() => controllerUser.value?.role === "admin");

export function useSession() {
  const lock = () => {
    controllerUser.value = null;
    controllerError.value = "";
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

  return { controllerUser, controllerError, canOperate, canAdmin, lock, login, logout, restore };
}
