import { afterEach, describe, expect, it, vi } from "vitest";
import { mount } from "@vue/test-utils";
import App from "./App.vue";
import router from "./router";
import { useSession } from "./session";

describe("App authentication gate", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    useSession().lock();
  });

  it("shows the Controller login before a session is established", () => {
    useSession().lock();
    const wrapper = mount(App, { global: { plugins: [router] } });
    expect(wrapper.find(".auth-shell").exists()).toBe(true);
    expect(wrapper.find(".controller-shell").exists()).toBe(false);
  });

  it("renders the Controller shell for an authenticated user", () => {
    const session = useSession();
    session.controllerUser.value = { id: "u1", username: "admin", role: "admin", enabled: true, revision: 1 };
    const wrapper = mount(App, { global: { plugins: [router] } });
    expect(wrapper.find(".controller-shell").exists()).toBe(true);
  });
});
