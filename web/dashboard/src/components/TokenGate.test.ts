import { describe, expect, it } from "vitest";
import { mount } from "@vue/test-utils";
import TokenGate from "./TokenGate.vue";

describe("TokenGate", () => {
  it("emits a trimmed Viewer token", async () => {
    const wrapper = mount(TokenGate, { props: { theme: "dark" } });
    await wrapper.find("input").setValue("  viewer-secret  ");
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("unlock")).toEqual([["viewer-secret"]]);
  });

  it("shows a Viewer authentication error without exposing the token", () => {
    const wrapper = mount(TokenGate, {
      props: { theme: "dark", error: "Viewer token 已失效，请重新解锁。" },
    });
    expect(wrapper.text()).toContain("Viewer token 已失效，请重新解锁。");
    expect(wrapper.text()).not.toContain("secret-token");
  });
});
