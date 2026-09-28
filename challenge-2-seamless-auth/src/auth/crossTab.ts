/**
 * Coordination between tabs of the same browser.
 *
 * Why it matters: the refresh token rotates on every use, and the backend
 * treats reuse of an old refresh token as theft and revokes the whole
 * session. If two tabs refresh at the same moment, the second one sends a
 * token the first already rotated, and the student is logged out
 * everywhere. So only one tab may refresh at a time, and the others adopt
 * its result.
 */

export type AuthMessage =
  | { type: 'token'; accessToken: string; expiresAt: number; issuedAt: number }
  | { type: 'session-expired'; at: number }
  | { type: 'logout' };

export interface TabChannel {
  post(msg: AuthMessage): void;
  subscribe(fn: (msg: AuthMessage) => void): () => void;
}

/** Runs fn while holding a lock shared by every tab of the origin. */
export interface TabLock {
  run<T>(name: string, fn: () => Promise<T>): Promise<T>;
}

/**
 * Time of the last successful refresh by any tab. It is read synchronously
 * inside the lock, so a tab can tell "another tab just refreshed; its
 * broadcast is on the way" apart from "nobody has refreshed". Only the
 * timestamp is shared, never the token.
 */
export interface SharedClock {
  lastRefreshAt(): number;
  markRefreshed(at: number): void;
}

export function broadcastChannel(name = 'youpass-auth'): TabChannel {
  if (typeof BroadcastChannel === 'undefined') {
    return { post() {}, subscribe: () => () => {} };
  }
  const ch = new BroadcastChannel(name);
  return {
    post: (msg) => ch.postMessage(msg),
    subscribe(fn) {
      const handler = (e: MessageEvent<AuthMessage>) => fn(e.data);
      ch.addEventListener('message', handler);
      return () => ch.removeEventListener('message', handler);
    },
  };
}

/**
 * Web Locks API (all current browsers). Without it we fall back to an
 * in-tab mutex and rely on the backend's reuse grace window instead.
 */
export function webLock(): TabLock {
  const locks = typeof navigator !== 'undefined' ? navigator.locks : undefined;
  if (locks) {
    return {
      run<T>(name: string, fn: () => Promise<T>): Promise<T> {
        // The lock is held until fn's promise settles.
        return locks.request(name, fn) as Promise<T>;
      },
    };
  }
  return inTabLock();
}

export function inTabLock(): TabLock {
  let tail: Promise<unknown> = Promise.resolve();
  return {
    run<T>(_name: string, fn: () => Promise<T>): Promise<T> {
      const result = tail.then(fn, fn);
      tail = result.catch(() => {});
      return result;
    },
  };
}

export function localStorageClock(key = 'youpass-auth:refreshed-at'): SharedClock {
  let fallback = 0;
  return {
    lastRefreshAt() {
      try {
        return Number(localStorage.getItem(key)) || 0;
      } catch {
        return fallback;
      }
    },
    markRefreshed(at) {
      fallback = at;
      try {
        localStorage.setItem(key, String(at));
      } catch {
        // Private mode or storage disabled: the in-tab value still works.
      }
    },
  };
}
