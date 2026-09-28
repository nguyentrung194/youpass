import { SessionExpiredError } from './errors';
import type { TokenManager } from './tokenManager';

export interface AuthRequestInit extends RequestInit {
  /**
   * Refresh first if the token has less validity than this. Use it before
   * long uploads (speaking recordings) so the token cannot expire mid-upload
   * and force a costly re-upload.
   */
  minTokenValidityMs?: number;
  /** For endpoints that must never trigger a refresh or replay (login, refresh, logout). */
  skipAuth?: boolean;
}

export interface AuthFetchOptions {
  fetch?: typeof fetch;
  /**
   * Whether a response means "access token refused". The default is any
   * 401; a backend that sends `WWW-Authenticate: Bearer error="invalid_token"`
   * can be matched more precisely.
   */
  isTokenRejected?: (res: Response) => boolean;
}

export type AuthFetch = (input: RequestInfo | URL, init?: AuthRequestInit) => Promise<Response>;

/**
 * fetch with the access token attached, and one transparent replay when the
 * token turns out to be expired:
 *
 *   request → 401 → refresh (shared with every other request that got a 401)
 *           → replay the same request with the new token → caller gets 200
 *
 * The caller (TanStack Query, a form submit) never sees the 401. A second 401
 * after a refresh is returned as-is, so a bad token can never cause a loop.
 */
export function createAuthFetch(tokens: TokenManager, options: AuthFetchOptions = {}): AuthFetch {
  const baseFetch = options.fetch ?? ((input, init) => fetch(input, init));
  const isTokenRejected = options.isTokenRejected ?? ((res) => res.status === 401);

  return async function authFetch(input, init = {}) {
    const { minTokenValidityMs, skipAuth, ...requestInit } = init;
    if (skipAuth) return baseFetch(input, requestInit);

    const signal = requestInit.signal ?? (input instanceof Request ? input.signal : undefined);
    // A stream body can only be sent once, so it cannot be replayed.
    const replayable = !(requestInit.body instanceof ReadableStream);

    const send = (token: string) => {
      // A Request body can be read only once: send a clone and keep the
      // original for the replay.
      const target = input instanceof Request ? input.clone() : input;
      const headers = new Headers(input instanceof Request ? input.headers : undefined);
      new Headers(requestInit.headers).forEach((value, key) => headers.set(key, value));
      headers.set('Authorization', `Bearer ${token}`);
      return baseFetch(target, { ...requestInit, headers });
    };

    const token = await abortable(tokens.getAccessToken(minTokenValidityMs), signal);
    const res = await send(token);
    if (!replayable || !isTokenRejected(res)) return res;

    void res.body?.cancel().catch(() => {});

    let fresh: string;
    try {
      fresh = await abortable(tokens.refresh(token), signal);
    } catch (err) {
      if (!(err instanceof SessionExpiredError)) throw err; // network: keep session, surface error
      // The session is really over. Park the request until the user signs
      // in again in the modal, then continue as if nothing happened.
      fresh = await tokens.waitForToken(undefined, signal);
    }
    return send(fresh);
  };
}

function abortable<T>(promise: Promise<T>, signal?: AbortSignal | null): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) return Promise.reject(signal.reason);
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(signal.reason);
    signal.addEventListener('abort', onAbort, { once: true });
    promise.then(
      (v) => (signal.removeEventListener('abort', onAbort), resolve(v)),
      (e) => (signal.removeEventListener('abort', onAbort), reject(e)),
    );
  });
}
