import { describe, expect, it } from "vitest";
import { useSession, viewerTokenErrorMessage } from "./session";

describe("dashboard session", () => {
  it("keeps the Viewer failure visible after invalidation and clears it on unlock", () => {
    const session = useSession();
    session.unlock("viewer-token");
    session.invalidateViewer();

    expect(session.viewerToken.value).toBe("");
    expect(session.adminToken.value).toBe("");
    expect(session.viewerError.value).toBe(viewerTokenErrorMessage);

    session.unlock("next-viewer-token");
    expect(session.viewerToken.value).toBe("next-viewer-token");
    expect(session.viewerError.value).toBe("");
    session.lock();
  });
});
