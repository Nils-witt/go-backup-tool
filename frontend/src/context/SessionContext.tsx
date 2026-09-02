import { createContext, useContext, type ReactNode } from "react";
import { useSessionInfo, type SessionInfoState } from "../hooks/useSessionInfo";

const SessionContext = createContext<SessionInfoState | null>(null);

// SessionProvider fetches the current session's permissions once at the app
// root and shares them with every page/nav item, rather than each having
// its own useSessionInfo() call hit /api/session independently.
export function SessionProvider({ children }: { children: ReactNode }) {
  const session = useSessionInfo();
  return <SessionContext.Provider value={session}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionInfoState {
  const ctx = useContext(SessionContext);
  if (!ctx) throw new Error("useSession must be used within a SessionProvider");
  return ctx;
}
