import { afterEach, describe, expect, it } from "vitest";
import { mount } from "@vue/test-utils";
import { nextTick } from "vue";
import App from "./App.vue";
import router from "./router";
import { useSession } from "./session";

describe("App authentication gate", () => {
  afterEach(() => {
    useSession().lock();
  });

  it("shows the token gate before a Viewer token is unlocked", () => {
    useSession().lock();
    const wrapper = mount(App, { global: { plugins: [router] } });

    expect(wrapper.find(".auth-shell").exists()).toBe(true);
    expect(wrapper.find(".app-shell").exists()).toBe(false);
  });

  it("keeps a Viewer authentication failure visible after locking", async () => {
    const session = useSession();
    const wrapper = mount(App, { global: { plugins: [router] } });

    session.invalidateViewer();
    await nextTick();

    expect(wrapper.find(".auth-error").text()).toContain("Viewer token 已失效");
  });
});
