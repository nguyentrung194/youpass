import { afterEach, describe, expect, it, vi } from 'vitest';
import { createAuthFetch } from './authFetch';
import { inTabLock, type AuthMessage, type SharedClock, type TabChannel, type TabLock } from './crossTab';
import { LoggedOutError, RefreshRejectedError } from './errors';
import { TokenManager, type TokenManagerOptions } from './tokenManager';

const API = 'https://api.youpass.vn';

/**
 * Fake backend with the real rules: access tokens can expire, refresh
 * tokens rotate on every use, and reusing an old refresh token revokes the
 * whole session (reuse detection).
 */
function createBackend({ lifetimeSec = 600, refreshDelayMs = 5 } = {}) {
  let seq = 0;
  let refreshCookie: string | null = 'rt-0';
  let presentedCookie: string | null = 'rt-0'; // what the browser will send next
  const validAccess = new Set(['at-0']);
  const failures: Array<'network' | 'reject'> = [];
  const stats = { refreshCalls: 0, unauthorized: 0, reuseDetected: false };

  const refresh = async () => {
    stats.refreshCalls++;
    const sent = presentedCookie; // cookie captured when the request leaves the browser
    await new Promise((r) => setTimeout(r, refreshDelayMs));
    const failure = failures.shift();
    if (failure === 'network') throw new TypeError('Failed to fetch');
    if (failure === 'reject' || refreshCookie === null) throw new RefreshRejectedError();
    if (sent !== refreshCookie) {
      stats.reuseDetected = true; // an already-rotated token was replayed: revoke everything
      refreshCookie = null;
      throw new RefreshRejectedError();
    }
    seq++;
    refreshCookie = presentedCookie = `rt-${seq}`;
    validAccess.clear();
    validAccess.add(`at-${seq}`);
    return { accessToken: `at-${seq}`, expiresIn: lifetimeSec };
  };

  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = new Request(input, init);
    const token = req.headers.get('Authorization')?.replace('Bearer ', '');
    if (!token || !validAccess.has(token)) {
      stats.unauthorized++;
      return new Response(null, { status: 401 });
    }
    return Response.json({ path: new URL(req.url).pathname, token, body: await req.text() });
  };

  return {
    refresh,
    fetch: fetchImpl as typeof fetch,
    stats,
    failNext: (...f: Array<'network' | 'reject'>) => failures.push(...f),
    /** The access token expires server-side (e.g. the student idled). */
    expireAccessTokens: () => validAccess.clear(),
    /** Server-side revocation, e.g. password changed on another device. */
    revokeSession: () => (refreshCookie = null),
    login() {
      seq++;
      refreshCookie = presentedCookie = `rt-${seq}`;
      validAccess.clear();
      validAccess.add(`at-${seq}`);
      return { accessToken: `at-${seq}`, expiresIn: lifetimeSec };
    },
  };
}

/** Tabs of one browser: shared lock, shared clock, async broadcast delivery. */
function createBrowser() {
  const lock: TabLock = inTabLock();
  let lastRefresh = 0;
  const clock: SharedClock = { lastRefreshAt: () => lastRefresh, markRefreshed: (at) => (lastRefresh = at) };
  const subscribers = new Set<(m: AuthMessage) => void>();
  const channel = (): TabChannel => {
    const mine = new Set<(m: AuthMessage) => void>();
    return {
      post(msg) {
        // Like BroadcastChannel: delivered later, and never to the sender.
        const targets = [...subscribers].filter((s) => !mine.has(s));
        setTimeout(() => targets.forEach((s) => s(structuredClone(msg))), 1);
      },
      subscribe(fn) {
        mine.add(fn);
        subscribers.add(fn);
        return () => (subscribers.delete(fn), mine.delete(fn));
      },
    };
  };
  return { lock, clock, channel };
}

const managers: TokenManager[] = [];
afterEach(() => {
  managers.splice(0).forEach((m) => m.dispose());
  vi.useRealTimers();
});

function newTab(
  backend: ReturnType<typeof createBackend>,
  browser = createBrowser(),
  extra: Partial<TokenManagerOptions> = {},
) {
  const tokens = new TokenManager({
    refresh: backend.refresh,
    lock: browser.lock,
    clock: browser.clock,
    channel: browser.channel(),
    retryBaseDelayMs: 1,
    ...extra,
  });
  managers.push(tokens);
  return { tokens, authFetch: createAuthFetch(tokens, { fetch: backend.fetch }) };
}

describe('seamless refresh', () => {
  it('turns many concurrent 401s into one refresh and replays every request', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 600);
    backend.expireAccessTokens();

    const results = await Promise.all(
      Array.from({ length: 10 }, (_, i) => authFetch(`${API}/lessons/${i}`).then((r) => r.json())),
    );

    expect(backend.stats.refreshCalls).toBe(1);
    expect(results.every((r) => r.token === 'at-1')).toBe(true);
    expect(tokens.getStatus()).toBe('authenticated');
  });

  it('replays POST bodies, including Request objects', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 600);
    backend.expireAccessTokens();

    const essay = 'Some people believe that...';
    const viaInit = await authFetch(`${API}/submissions`, { method: 'POST', body: essay }).then((r) => r.json());
    backend.expireAccessTokens();
    const req = new Request(`${API}/submissions`, { method: 'POST', body: essay });
    const viaRequest = await authFetch(req).then((r) => r.json());

    expect(viaInit.body).toBe(essay);
    expect(viaRequest.body).toBe(essay);
  });

  it('refreshes ahead of expiry so requests never see a 401', async () => {
    vi.useFakeTimers();
    const backend = createBackend({ lifetimeSec: 300, refreshDelayMs: 0 });
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 300);

    await vi.advanceTimersByTimeAsync(250_000); // refresh fires ~60s before expiry
    expect(backend.stats.refreshCalls).toBe(1);

    await vi.advanceTimersByTimeAsync(100_000); // at-0 would have expired by now
    const res = await authFetch(`${API}/me`).then((r) => r.json());
    expect(res.token).toBe('at-1');
    expect(backend.stats.unauthorized).toBe(0);
  });

  it('refreshes before a long upload if the token would expire during it', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 120); // 2 minutes left

    const res = await authFetch(`${API}/speaking/upload`, {
      method: 'POST',
      body: 'audio-bytes',
      minTokenValidityMs: 5 * 60_000,
    }).then((r) => r.json());

    expect(res.token).toBe('at-1');
    expect(backend.stats.unauthorized).toBe(0);
  });

  it('retries transient refresh failures and never logs out because of the network', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 600);
    backend.expireAccessTokens();

    backend.failNext('network', 'network');
    await expect(authFetch(`${API}/me`).then((r) => r.status)).resolves.toBe(200);
    expect(backend.stats.refreshCalls).toBe(3);

    backend.expireAccessTokens();
    backend.failNext('network', 'network', 'network');
    await expect(authFetch(`${API}/me`)).rejects.toThrow('Failed to fetch');
    expect(tokens.getStatus()).toBe('authenticated'); // still signed in; the next request retries
  });

  it('parks requests when the session is over and resumes them after re-login', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 600);
    backend.expireAccessTokens();
    backend.revokeSession();

    const statuses: string[] = [];
    tokens.subscribe(() => statuses.push(tokens.getStatus()));
    const submit = authFetch(`${API}/submissions`, { method: 'POST', body: 'my essay' });

    await vi.waitFor(() => expect(tokens.getStatus()).toBe('session-expired'));
    // The UI shows the re-login modal over the page; the submit is still pending.
    const { accessToken, expiresIn } = backend.login();
    tokens.setSession(accessToken, expiresIn);

    const res = await submit.then((r) => r.json());
    expect(res.body).toBe('my essay');
    expect(statuses).toEqual(['session-expired', 'authenticated']);
  });

  it('never replays twice: a 401 after a successful refresh reaches the caller', async () => {
    const backend = createBackend();
    const alwaysDenied = (async () => new Response(null, { status: 401 })) as typeof fetch;
    const { tokens } = newTab(backend);
    const authFetch = createAuthFetch(tokens, { fetch: alwaysDenied });
    tokens.setSession('at-0', 600);

    const res = await authFetch(`${API}/admin`);
    expect(res.status).toBe(401);
    expect(backend.stats.refreshCalls).toBe(1);
  });

  it('lets an abort signal cancel a parked request', async () => {
    const backend = createBackend();
    const { tokens, authFetch } = newTab(backend);
    tokens.setSession('at-0', 600);
    backend.expireAccessTokens();
    backend.revokeSession();

    const ctrl = new AbortController();
    const pending = authFetch(`${API}/me`, { signal: ctrl.signal });
    await vi.waitFor(() => expect(tokens.getStatus()).toBe('session-expired'));
    ctrl.abort();
    await expect(pending).rejects.toThrow();
  });

  it('treats a missing session on page load as logged out, not expired', async () => {
    const backend = createBackend();
    backend.revokeSession();
    const { tokens } = newTab(backend);
    await expect(tokens.init()).resolves.toBe('logged-out');
    await expect(tokens.getAccessToken()).rejects.toBeInstanceOf(LoggedOutError);
  });
});

describe('multiple tabs', () => {
  it('refreshes once across tabs, so refresh-token rotation never trips reuse detection', async () => {
    const backend = createBackend({ refreshDelayMs: 20 });
    const browser = createBrowser();
    const tabs = [newTab(backend, browser), newTab(backend, browser), newTab(backend, browser)];
    tabs.forEach((t) => t.tokens.setSession('at-0', 600));
    backend.expireAccessTokens();

    const results = await Promise.all(tabs.map((t) => t.authFetch(`${API}/me`).then((r) => r.json())));

    expect(backend.stats.reuseDetected).toBe(false);
    expect(backend.stats.refreshCalls).toBe(1);
    expect(results.map((r) => r.token)).toEqual(['at-1', 'at-1', 'at-1']);
  });

  it('shares re-login and logout with every tab', async () => {
    const backend = createBackend();
    const browser = createBrowser();
    const a = newTab(backend, browser);
    const b = newTab(backend, browser);
    a.tokens.setSession('at-0', 600);
    await vi.waitFor(() => expect(b.tokens.getStatus()).toBe('authenticated'));

    backend.expireAccessTokens();
    backend.revokeSession();
    const parkedInB = b.authFetch(`${API}/me`);
    await vi.waitFor(() => expect(a.tokens.getStatus()).toBe('session-expired'));

    const session = backend.login(); // student signs in again from tab A's modal
    a.tokens.setSession(session.accessToken, session.expiresIn);
    await expect(parkedInB.then((r) => r.status)).resolves.toBe(200);

    a.tokens.logout();
    await vi.waitFor(() => expect(b.tokens.getStatus()).toBe('logged-out'));
  });
});
