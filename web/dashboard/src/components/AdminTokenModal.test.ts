import { afterEach, describe, expect, it } from "vitest";
import { mount } from "@vue/test-utils";
import { nextTick } from "vue";
import AdminTokenModal from "./AdminTokenModal.vue";

describe("AdminTokenModal", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("emits the Admin token entered for a write action", async () => {
    const wrapper = mount(AdminTokenModal, { props: { visible: true }, attachTo: document.body });
    await nextTick();
    const input = document.body.querySelector("input") as HTMLInputElement | null;
    expect(input).not.toBeNull();
    if (input) {
      input.value = "admin-secret";
      input.dispatchEvent(new Event("input", { bubbles: true }));
    }
    await nextTick();
    const button = document.body.querySelector(".modal-actions .arco-btn-primary") as HTMLButtonElement | null;
    expect(button).not.toBeNull();
    button?.click();
    await nextTick();
    expect(wrapper.emitted("confirm")).toEqual([["admin-secret"]]);
  });
});
