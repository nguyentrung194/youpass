import {
  broadcastChannel,
  localStorageClock,
  webLock,
  type AuthMessage,
  type SharedClock,
  type TabChannel,
  type TabLock,
} from './crossTab';
import { LoggedOutError, RefreshRejectedError, SessionExpiredError } from './errors';

export type AuthStatus = 'unknown' | 'authenticated' | 'session-expired' | 'logged-out';

export interface RefreshResponse {
  accessToken: string;
  /** Lifetime in seconds. Used instead of the JWT `exp` so a wrong device clock does not matter. */
  expiresIn: number;
}

export interface TokenManagerOptions {
  /**
   * Calls POST /auth/refresh. The refresh token travels in an httpOnly cookie,
   * so JavaScript never sees it. Must throw RefreshRejectedError when the
   * server rejects the refresh token (401/403); any other error is treated as
   * transient and retried.
   */
  refresh: () => Promise<RefreshResponse>;
  lock?: TabLock;
  channel?: TabChannel;
  clock?: SharedClock;
  now?: () => number;
  /** Refresh in the background when less than this remains. Default 60s. */
  refreshAheadMs?: number;
  /** Refresh before sending a request if the token has less than this left. Default 10s. */
  minValidityMs?: number;
  retryAttempts?: number;
  retryBaseDelayMs?: number;
  /** How long to wait for another tab's broadcast after it refreshed. */
  peerWaitMs?: number;
  /** A refresh by another tab more recent than this is assumed to be on its way to us. */
  peerRecentMs?: number;
}

interface Waiter {
  resolve: (token: string) => void;
  reject: (err: unknown) => void;
}

const LOCK_NAME = 'youpass-auth-refresh';

/**
 * Owns the access token for one tab.
 *
 * - The access token lives only in memory (never in localStorage), so an
 *   XSS payload cannot copy a long-lived credential.
 * - Refresh is single-flight within a tab and serialized across tabs.
 * - Tokens are refreshed ahead of expiry, so most requests never see a 401;
 *   the 401 replay in authFetch is the safety net.
 * - When the session is really over, requests wait for the user to sign in
 *   again in a modal instead of failing, so unsaved work is not lost.
 */
export class TokenManager {
  private accessToken: string | null = null;
  private expiresAt = 0;
  /** When the current token was obtained. Orders tokens that arrive from other tabs. */
  private issuedAt = 0;
  private status: AuthStatus = 'unknown';
  private inflight: Promise<string> | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private readonly listeners = new Set<() => void>();
  private readonly waiters = new Set<Waiter>();
  private readonly unsubscribe: () => void;

  private readonly opts: Required<TokenManagerOptions>;

  constructor(options: TokenManagerOptions) {
    this.opts = {
      lock: webLock(),
      channel: broadcastChannel(),
      clock: localStorageClock(),
      now: () => Date.now(),
      refreshAheadMs: 60_000,
      minValidityMs: 10_000,
      retryAttempts: 3,
      retryBaseDelayMs: 300,
      peerWaitMs: 1_500,
      peerRecentMs: 5_000,
      ...options,
    };
    this.unsubscribe = this.opts.channel.subscribe((msg) => this.onMessage(msg));
  }

  // ---- React bindings (useSyncExternalStore) ----

  getStatus = (): AuthStatus => this.status;

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  // ---- Session lifecycle ----

  /** Restores the session on page load from the refresh cookie. */
  async init(): Promise<AuthStatus> {
    if (this.status !== 'unknown') return this.status;
    try {
      await this.refresh();
    } catch {
      // Either nobody is signed in (status becomes logged-out) or the
      // network is down (status stays unknown; the next request retries).
    }
    return this.status;
  }

  /** Call after login, including re-login from the "session expired" modal. */
  setSession(accessToken: string, expiresIn: number): void {
    const at = this.opts.now();
    this.adopt(accessToken, at + expiresIn * 1000, at, true);
  }

  logout(): void {
    this.clear('logged-out');
    this.opts.channel.post({ type: 'logout' });
  }

  dispose(): void {
    this.unsubscribe();
    clearTimeout(this.timer);
  }

  // ---- Tokens ----

  /**
   * Returns a token valid for at least minValidityMs, refreshing first if
   * needed. Pass a large value before a long upload (e.g. a speaking
   * recording) so the token cannot expire halfway through.
   */
  async getAccessToken(minValidityMs = this.opts.minValidityMs): Promise<string> {
    if (this.status === 'logged-out') throw new LoggedOutError();
    if (this.status === 'session-expired') return this.waitForToken();
    if (this.accessToken && this.remaining() > minValidityMs) return this.accessToken;
    return this.refresh();
  }

  /**
   * Gets a new access token. However many callers ask at once, at most one
   * refresh request is in flight per tab, and at most one across tabs.
   *
   * @param rejected the token the server just refused. If a newer token
   *   already exists, another request refreshed in the meantime and it is
   *   returned without calling the server again.
   */
  refresh(rejected?: string): Promise<string> {
    if (rejected !== undefined && this.accessToken && this.accessToken !== rejected && this.remaining() > 0) {
      return Promise.resolve(this.accessToken);
    }
    if (this.status === 'session-expired') return this.waitForToken();
    if (this.status === 'logged-out') return Promise.reject(new LoggedOutError());
    this.inflight ??= this.refreshAcrossTabs().finally(() => {
      this.inflight = null;
    });
    return this.inflight;
  }

  /**
   * Resolves with the next token this tab adopts: the user's re-login, or a
   * refresh broadcast by another tab. Rejects on logout, on timeout, or when
   * the signal aborts.
   */
  waitForToken(timeoutMs?: number, signal?: AbortSignal | null): Promise<string> {
    return new Promise<string>((resolve, reject) => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      const cleanup = () => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', onAbort);
        this.waiters.delete(waiter);
      };
      const waiter: Waiter = {
        resolve: (t) => (cleanup(), resolve(t)),
        reject: (e) => (cleanup(), reject(e)),
      };
      const onAbort = () => waiter.reject(signal?.reason ?? new DOMException('Aborted', 'AbortError'));
      if (signal?.aborted) return onAbort();
      signal?.addEventListener('abort', onAbort, { once: true });
      if (timeoutMs !== undefined) {
        timer = setTimeout(() => waiter.reject(new Error('timed out waiting for token')), timeoutMs);
      }
      this.waiters.add(waiter);
    });
  }

  /**
   * Browsers throttle timers in background tabs and freeze them while a
   * laptop sleeps, so the scheduled refresh can fire late. Check again
   * whenever the tab wakes up.
   */
  startBackgroundRefresh(win: Window = window): () => void {
    const wake = () => {
      if (win.document.visibilityState === 'visible') this.onWake();
    };
    win.document.addEventListener('visibilitychange', wake);
    win.addEventListener('online', wake);
    win.addEventListener('focus', wake);
    return () => {
      win.document.removeEventListener('visibilitychange', wake);
      win.removeEventListener('online', wake);
      win.removeEventListener('focus', wake);
    };
  }

  onWake(): void {
    if (this.status === 'authenticated' && this.remaining() <= this.opts.refreshAheadMs) {
      this.refresh().catch(() => {});
    }
  }

  // ---- Internals ----

  private refreshAcrossTabs(): Promise<string> {
    const seenIssuedAt = this.issuedAt;
    const wasUnknown = this.status === 'unknown';

    return this.opts.lock
      .run(LOCK_NAME, async () => {
        // Another tab may have refreshed while we waited for the lock and
        // its broadcast has already reached us.
        if (this.issuedAt > seenIssuedAt && this.accessToken && this.remaining() > this.opts.minValidityMs) {
          return this.accessToken;
        }
        // Or it refreshed a moment ago and the broadcast is still on the
        // way. Refreshing ourselves would reuse the refresh token it just
        // rotated, which the backend treats as theft.
        const peerAt = this.opts.clock.lastRefreshAt();
        if (peerAt > this.issuedAt && this.opts.now() - peerAt < this.opts.peerRecentMs) {
          const token = await this.waitForToken(this.opts.peerWaitMs).catch(() => null);
          if (token) return token;
        }

        const res = await this.callRefreshWithRetry();
        const at = this.opts.now();
        // adopt() records the refresh in the shared clock before the lock is released.
        this.adopt(res.accessToken, at + res.expiresIn * 1000, at, true);
        return res.accessToken;
      })
      .catch((err: unknown) => {
        if (!(err instanceof RefreshRejectedError)) throw err; // transient: keep the session
        if (wasUnknown) {
          this.setStatus('logged-out'); // page load with no session: not an "expiry"
          throw new LoggedOutError();
        }
        this.expireSession(true);
        throw new SessionExpiredError();
      });
  }

  private async callRefreshWithRetry(): Promise<RefreshResponse> {
    for (let attempt = 1; ; attempt++) {
      try {
        return await this.opts.refresh();
      } catch (err) {
        if (err instanceof RefreshRejectedError || attempt >= this.opts.retryAttempts) throw err;
        const base = this.opts.retryBaseDelayMs * 2 ** (attempt - 1);
        await sleep(base + Math.random() * (base / 2));
      }
    }
  }

  private adopt(token: string, expiresAt: number, issuedAt: number, broadcast: boolean): void {
    if (issuedAt < this.issuedAt) return; // a slower broadcast carrying an older token
    this.accessToken = token;
    this.expiresAt = expiresAt;
    this.issuedAt = issuedAt;
    this.setStatus('authenticated');
    this.schedule();
    if (broadcast) {
      this.opts.clock.markRefreshed(issuedAt);
      this.opts.channel.post({ type: 'token', accessToken: token, expiresAt, issuedAt });
    }
    for (const w of [...this.waiters]) w.resolve(token);
  }

  private onMessage(msg: AuthMessage): void {
    switch (msg.type) {
      case 'token':
        this.adopt(msg.accessToken, msg.expiresAt, msg.issuedAt, false);
        break;
      case 'session-expired':
        if (this.status === 'authenticated' && this.issuedAt <= msg.at) this.expireSession(false);
        break;
      case 'logout':
        this.clear('logged-out');
        break;
    }
  }

  /** Session over, but requests keep waiting for a re-login. */
  private expireSession(broadcast: boolean): void {
    this.accessToken = null;
    this.expiresAt = 0;
    clearTimeout(this.timer);
    this.setStatus('session-expired');
    if (broadcast) this.opts.channel.post({ type: 'session-expired', at: this.opts.now() });
  }

  private clear(status: AuthStatus): void {
    this.accessToken = null;
    this.expiresAt = 0;
    this.issuedAt = 0;
    clearTimeout(this.timer);
    this.setStatus(status);
    for (const w of [...this.waiters]) w.reject(new LoggedOutError());
  }

  private schedule(): void {
    clearTimeout(this.timer);
    const lifetime = this.expiresAt - this.issuedAt;
    // Short-lived tokens refresh at half-life. The jitter spreads tabs out
    // so they do not all queue on the lock at the same moment.
    const ahead = Math.min(this.opts.refreshAheadMs, lifetime / 2);
    const jitter = Math.random() * Math.min(5_000, lifetime / 10);
    this.refreshAfter(Math.max(0, this.expiresAt - ahead - jitter - this.opts.now()));
  }

  // adopt() reschedules on every new token, so when this timer fires nobody
  // has refreshed since it was set.
  private refreshAfter(delay: number): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      if (this.status !== 'authenticated') return;
      this.refresh().catch(() => {
        // Offline or server error: try again soon. Requests can still
        // refresh on demand in the meantime.
        if (this.status === 'authenticated') this.refreshAfter(15_000);
      });
    }, delay);
  }

  private remaining(): number {
    return this.expiresAt - this.opts.now();
  }

  private setStatus(status: AuthStatus): void {
    if (this.status === status) return;
    this.status = status;
    for (const fn of [...this.listeners]) fn();
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
