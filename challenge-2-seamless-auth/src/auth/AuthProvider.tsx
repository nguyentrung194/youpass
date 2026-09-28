import { createContext, useContext, useEffect, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { createAuthFetch, type AuthFetch } from './authFetch';
import type { AuthStatus, TokenManager } from './tokenManager';

interface AuthContextValue {
  tokens: TokenManager;
  authFetch: AuthFetch;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({
  tokens,
  renderReLogin,
  children,
}: {
  tokens: TokenManager;
  /** Modal shown when the session is really over. It must call tokens.setSession() after login. */
  renderReLogin: () => ReactNode;
  children: ReactNode;
}) {
  const status = useSyncExternalStore(tokens.subscribe, tokens.getStatus, tokens.getStatus);
  const value = useMemo(() => ({ tokens, authFetch: createAuthFetch(tokens) }), [tokens]);

  useEffect(() => {
    void tokens.init();
    return tokens.startBackgroundRefresh(window);
  }, [tokens]);

  return (
    <AuthContext.Provider value={value}>
      {children}
      {/* Rendered over the page, never instead of it: the essay the student
          is writing stays mounted, and parked requests resume after login. */}
      {status === 'session-expired' && renderReLogin()}
    </AuthContext.Provider>
  );
}

function useAuthContext(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth* must be used inside <AuthProvider>');
  return ctx;
}

export function useAuthFetch(): AuthFetch {
  return useAuthContext().authFetch;
}

export function useAuthStatus(): AuthStatus {
  const { tokens } = useAuthContext();
  return useSyncExternalStore(tokens.subscribe, tokens.getStatus, tokens.getStatus);
}

export function useTokens(): TokenManager {
  return useAuthContext().tokens;
}
