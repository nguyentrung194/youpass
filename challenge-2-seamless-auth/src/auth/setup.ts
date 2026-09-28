/**
 * Wiring for the YouPass web app. Backend contract assumed:
 *
 *   POST /api/auth/login    → { access_token, expires_in } + Set-Cookie: yp_rt (refresh token)
 *   POST /api/auth/refresh  → same shape; rotates yp_rt; 401 when yp_rt is invalid/revoked
 *   POST /api/auth/logout   → revokes the refresh token family, clears yp_rt
 *
 *   yp_rt: HttpOnly; Secure; SameSite=Strict; Path=/api/auth
 *   Access token: JWT, 10–15 min, sent as `Authorization: Bearer`.
 */
import { createAuthFetch } from './authFetch';
import { broadcastChannel, localStorageClock, webLock } from './crossTab';
import { RefreshRejectedError } from './errors';
import { TokenManager } from './tokenManager';

const API = '/api';

export const tokens = new TokenManager({
  async refresh() {
    const res = await fetch(`${API}/auth/refresh`, { method: 'POST', credentials: 'include' });
    if (res.status === 401 || res.status === 403) throw new RefreshRejectedError();
    if (!res.ok) throw new Error(`refresh failed: HTTP ${res.status}`); // transient, retried
    const body = (await res.json()) as { access_token: string; expires_in: number };
    return { accessToken: body.access_token, expiresIn: body.expires_in };
  },
  lock: webLock(),
  channel: broadcastChannel('youpass-auth'),
  clock: localStorageClock(),
});

export const authFetch = createAuthFetch(tokens);

export async function login(email: string, password: string): Promise<void> {
  const res = await fetch(`${API}/auth/login`, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  });
  if (!res.ok) throw new Error('login failed');
  const body = (await res.json()) as { access_token: string; expires_in: number };
  tokens.setSession(body.access_token, body.expires_in); // also wakes parked requests in every tab
}

export async function logout(): Promise<void> {
  tokens.logout();
  await fetch(`${API}/auth/logout`, { method: 'POST', credentials: 'include' }).catch(() => {});
}
