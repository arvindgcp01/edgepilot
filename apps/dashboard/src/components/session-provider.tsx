"use client";

/**
 * Session state, held once at the root.
 *
 * The access token lives in the API client's module scope rather than in React
 * state — it is not rendered, and putting it in state would mean every consumer
 * re-renders when it rotates every ten minutes.
 */
import { createContext, useContext, useEffect, useState } from "react";
import {
  bootstrapSession,
  onSessionChange,
  scheduleRefresh,
  type Principal,
} from "@/lib/api/client";

type SessionState = {
  principal: Principal | null;
  /** Distinguishes "not signed in" from "we do not know yet", which is the
   *  difference between showing a login form and flashing one at a signed-in
   *  operator on every reload. */
  loading: boolean;
};

const SessionContext = createContext<SessionState>({ principal: null, loading: true });

export function useSession(): SessionState {
  return useContext(SessionContext);
}

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [principal, setPrincipal] = useState<Principal | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const unsubscribe = onSessionChange(setPrincipal);
    // The access token is in memory, so a reload has none. The refresh cookie
    // survives; this turns it back into a session.
    void bootstrapSession().finally(() => setLoading(false));
    const stopRefresh = scheduleRefresh(900);
    return () => {
      unsubscribe();
      stopRefresh();
    };
  }, []);

  return (
    <SessionContext.Provider value={{ principal, loading }}>
      {children}
    </SessionContext.Provider>
  );
}
