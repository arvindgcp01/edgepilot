import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, refreshSession, request, signIn } from "@/lib/api/client";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => vi.unstubAllGlobals());

describe("the API client", () => {
  it("surfaces Problem Details, including the request id", async () => {
    // A fresh Response per call: a body can only be read once, and reusing one
    // makes the second read look like a parse failure.
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse(422, {
      title: "Validation failed",
      detail: "expires_at must be in the future",
      status: 422,
      request_id: "req-123",
      errors: [{ pointer: "/expires_at", message: "must be in the future" }],
    }))));

    const failure = await request("/enrollment-tokens", { method: "POST", body: "{}" }, false)
      .then(() => null, (err: unknown) => err);

    expect(failure).toBeInstanceOf(ApiError);
    const api = failure as ApiError;
    // The detail is what an operator reads; the request id is what makes a
    // support conversation short.
    expect(api.message).toContain("must be in the future");
    expect(api.problem.request_id).toBe("req-123");
    expect(api.fieldErrors.expires_at).toBe("must be in the future");
  });

  it("never puts the access token anywhere JavaScript can persist it", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(jsonResponse(200, {
      access_token: "secret-token", token_type: "Bearer", expires_in: 900,
      principal: { user_id: "u", org_id: "o", email: "a@b.c", roles: [], permissions: [] },
    })));

    await signIn("a@b.c", "password");

    // An XSS that reads storage must find nothing. The token lives in a module
    // variable and dies with the page.
    expect(JSON.stringify(localStorage)).not.toContain("secret-token");
    expect(JSON.stringify(sessionStorage)).not.toContain("secret-token");
    expect(document.cookie).not.toContain("secret-token");
  });

  it("shares one refresh between concurrent callers", async () => {
    // Refresh tokens are single-use, and presenting a spent one revokes the
    // family. Five requests that all 401 must not spend five tokens.
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, {
      access_token: "t", token_type: "Bearer", expires_in: 900,
      principal: { user_id: "u", org_id: "o", email: "a@b.c", roles: [], permissions: [] },
    }));
    vi.stubGlobal("fetch", fetchMock);

    await Promise.all([refreshSession(), refreshSession(), refreshSession()]);

    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("sends credentials, or the refresh cookie never travels", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { data: [] }));
    vi.stubGlobal("fetch", fetchMock);

    await request("/devices", {}, false);

    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ credentials: "include" });
  });
});
