import { afterEach, describe, expect, it, vi } from "vitest";
import { controllerRequestTimeoutMs, currentUser } from "./controller-api";
import { useSession } from "./session";

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  useSession().lock();
});

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

  it("fails a stalled Controller request within the client timeout", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn().mockImplementation((_input: RequestInfo, init?: RequestInit) => new Promise((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
    })));
    const pending = currentUser();
    const assertion = expect(pending).rejects.toMatchObject({ status: 0, code: "request_timeout" });
    await vi.advanceTimersByTimeAsync(controllerRequestTimeoutMs);
    await assertion;
  });
});
