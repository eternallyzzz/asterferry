import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mount } from "@vue/test-utils";
import App from "./App.vue";
import router from "./router";
import { useSession } from "./session";

describe("App authentication gate", () => {
  const wrappers: Array<{ unmount: () => void }> = [];

  beforeEach(() => {
    // App mounts restore() and the authenticated shell starts a refresh loop.
    // Keep those lifecycle requests deterministic so tests never probe a
    // developer's local Controller (or leave a timer issuing requests after a
    // test has finished).
    vi.stubGlobal("fetch", vi.fn().mockImplementation(() => Promise.resolve(new Response(
      JSON.stringify({ error: { code: "unauthorized", message: "authentication is required" } }),
      { status: 401, headers: { "content-type": "application/json" } },
    ))));
  });

  afterEach(() => {
    for (const wrapper of wrappers.splice(0)) wrapper.unmount();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    useSession().lock();
  });

  it("shows the Controller login before a session is established", () => {
    useSession().lock();
    const wrapper = mount(App, { global: { plugins: [router] } });
    wrappers.push(wrapper);
    expect(wrapper.find(".auth-shell").exists()).toBe(true);
    expect(wrapper.find(".app-shell").exists()).toBe(false);
  });

  it("renders the Controller shell for an authenticated user", () => {
    const session = useSession();
    session.controllerUser.value = { id: "u1", username: "admin", role: "admin", enabled: true, revision: 1 };
    const wrapper = mount(App, { global: { plugins: [router] } });
    wrappers.push(wrapper);
    expect(wrapper.find(".app-shell").exists()).toBe(true);
  });
});
