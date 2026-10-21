/**
 * The API client.
 *
 * # Where the tokens live
 *
 * The access token is held **in memory only**. Not localStorage, not
 * sessionStorage, not a readable cookie: anything JavaScript can read, an XSS
 * can exfiltrate, and a token in localStorage survives the tab that leaked it.
 *
 * The refresh token is an HttpOnly cookie the browser attaches automatically
 * and this code can never see. That is the whole point — an XSS can *use* the
 * session while the page is open, which is bad, but it cannot steal a
 * credential that outlives the page, which is much worse.
 *
 * The cost is that a page reload has no access token. The fix is a silent
 * refresh on boot, which is what `bootstrapSession` does.
 *
 * # Why one refresh at a time
 *
 * Refresh tokens are single-use, and presenting a rotated one revokes the whole
 * family — the server treats it as a leak, correctly. Two requests that both
 * 401 must therefore not both refresh: the second would present a token the
 * first has already spent, and the user would be logged out for doing nothing
 * wrong. Every caller shares one in-flight refresh.
 */

export type Problem = {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  errors?: { pointer: string; message: string }[];
  request_id?: string;
};

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem;

  constructor(status: number, problem: Problem) {
    super(problem.detail || problem.title || `Request failed with ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }

  /** Field-level messages, for rendering next to the inputs that caused them. */
  get fieldErrors(): Record<string, string> {
    const out: Record<string, string> = {};
    for (const e of this.problem.errors ?? []) {
      out[e.pointer.replace(/^\//, "")] = e.message;
    }
    return out;
  }
}

export type Principal = {
  user_id: string;
  org_id: string;
  email: string;
  roles: string[];
  permissions: string[];
};

type TokenPair = {
  access_token: string;
  token_type: string;
  expires_in: number;
  principal: Principal;
};

/** The base URL, from the environment at build time. */
// Empty means same-origin, which is the only configuration in which the
// SameSite=Strict refresh cookie survives. See the rewrite in next.config.ts.
export const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL?.replace(/\/$/, "") ?? "";

let accessToken: string | null = null;
let principal: Principal | null = null;
let refreshInFlight: Promise<boolean> | null = null;

const listeners = new Set<(p: Principal | null) => void>();

/** Subscribe to sign-in and sign-out. */
export function onSessionChange(fn: (p: Principal | null) => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function setSession(pair: TokenPair | null) {
  accessToken = pair?.access_token ?? null;
  principal = pair?.principal ?? null;
  for (const fn of listeners) fn(principal);
}

export function currentPrincipal(): Principal | null {
  return principal;
}

export function can(permission: string): boolean {
  return principal?.permissions.includes(permission) ?? false;
}

/** The token, for the WebSocket — which cannot carry an Authorization header. */
export function currentAccessToken(): string | null {
  return accessToken;
}

async function parseProblem(response: Response): Promise<Problem> {
  try {
    return (await response.json()) as Problem;
  } catch {
    // A proxy 502 or a dropped connection produces no JSON. Say something
    // useful rather than "Unexpected token < in JSON".
    return { status: response.status, title: response.statusText || "Request failed" };
  }
}

/**
 * request performs one authenticated call, refreshing once on a 401.
 *
 * The retry is deliberately once. A second 401 after a successful refresh means
 * the session is genuinely gone — the account was disabled, or an administrator
 * revoked it — and retrying past that is a loop that hides the reason.
 */
export async function request<T>(
  path: string,
  init: RequestInit = {},
  retryOn401 = true,
): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body !== undefined && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  if (accessToken) headers.set("Authorization", `Bearer ${accessToken}`);

  const response = await fetch(`${API_BASE}/api/v1${path}`, {
    ...init,
    headers,
    // The refresh cookie is scoped to /api/v1/auth, so it travels only where it
    // is needed — but credentials must be included for it to travel at all.
    credentials: "include",
  });

  if (response.status === 401 && retryOn401) {
    if (await refreshSession()) {
      return request<T>(path, init, false);
    }
  }

  if (!response.ok) {
    throw new ApiError(response.status, await parseProblem(response));
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export async function signIn(
  email: string,
  password: string,
  extra: { mfa_code?: string; organization?: string } = {},
): Promise<Principal> {
  const response = await fetch(`${API_BASE}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    credentials: "include",
    body: JSON.stringify({ email, password, ...extra }),
  });
  if (!response.ok) {
    throw new ApiError(response.status, await parseProblem(response));
  }
  const pair = (await response.json()) as TokenPair;
  setSession(pair);
  return pair.principal;
}

export async function signOut(): Promise<void> {
  try {
    await request<void>("/auth/logout", { method: "POST" }, false);
  } finally {
    // Local state is cleared even if the call failed. A user who clicked sign
    // out and stayed signed in has been actively misled.
    setSession(null);
  }
}

/**
 * refreshSession exchanges the refresh cookie for a new access token.
 *
 * Shared: concurrent callers await the same request, so a page that fires five
 * requests on load does not spend five single-use refresh tokens and revoke its
 * own family.
 */
export function refreshSession(): Promise<boolean> {
  if (refreshInFlight) return refreshInFlight;

  refreshInFlight = (async () => {
    try {
      const response = await fetch(`${API_BASE}/api/v1/auth/refresh`, {
        method: "POST",
        headers: { Accept: "application/json" },
        credentials: "include",
      });
      if (!response.ok) {
        setSession(null);
        return false;
      }
      setSession((await response.json()) as TokenPair);
      return true;
    } catch {
      // The network is down. Keep whatever session we have rather than logging
      // the operator out because their wifi dropped.
      return false;
    } finally {
      refreshInFlight = null;
    }
  })();

  return refreshInFlight;
}

/**
 * bootstrapSession restores a session on page load.
 *
 * The access token lives in memory, so a reload has none. The refresh cookie
 * survives, and this is what turns it back into one — without it, every refresh
 * of the page would look like being logged out.
 */
export async function bootstrapSession(): Promise<Principal | null> {
  await refreshSession();
  return principal;
}

/**
 * scheduleRefresh keeps the access token alive while the tab is open.
 *
 * Fires at two thirds of the token's life rather than on expiry: a request that
 * races the deadline would otherwise 401, refresh, and retry — correct, but it
 * shows the user a stall for no reason.
 */
export function scheduleRefresh(expiresInSeconds: number): () => void {
  const delay = Math.max(30_000, (expiresInSeconds * 1000 * 2) / 3);
  const timer = setInterval(() => void refreshSession(), delay);
  return () => clearInterval(timer);
}
