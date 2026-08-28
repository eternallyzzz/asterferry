import { describe, expect, it, vi } from "vitest";
import { useSession } from "./session";

describe("dashboard session", () => {
  it("stores the Controller user after login and clears it on lock", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ user: { id: "u1", username: "admin", role: "admin", enabled: true, revision: 1 }, csrf_token: "csrf" }),
      { status: 200, headers: { "content-type": "application/json" } },
    )));
    const session = useSession();
    await session.login("admin", "password");
    expect(session.controllerUser.value?.role).toBe("admin");
    session.lock();
    expect(session.controllerUser.value).toBeNull();
  });
});
