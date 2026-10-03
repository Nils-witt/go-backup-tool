import type { ReactNode } from "react";
import Alert from "@mui/material/Alert";
import { useAuth, type AuthState } from "../auth/AuthContext";

// RequirePermission renders its children only once the account has loaded
// and `test` passes; otherwise it shows nothing (still loading) or an
// access-denied notice. The server enforces the same rule on every actual
// request behind each page — this is purely a display-time gate so a user
// without a permission doesn't see a page that will just 403 underneath.
export function RequirePermission({
  test,
  children,
}: {
  test: (auth: AuthState) => boolean;
  children: ReactNode;
}) {
  const auth = useAuth();

  if (!auth.me) return null;
  if (!test(auth)) {
    return <Alert severity="warning">You don't have permission to view this page.</Alert>;
  }

  return <>{children}</>;
}
