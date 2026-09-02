import type { ReactNode } from "react";
import Alert from "@mui/material/Alert";
import { useSession } from "../context/SessionContext";

// RequirePermission renders its children only once the session has loaded
// and `test` passes; otherwise it shows nothing (still loading) or an
// access-denied notice. The server enforces the same rule on every actual
// request behind each page — this is purely a display-time gate so a user
// without a permission doesn't see a page that will just 403 underneath.
export function RequirePermission({
  test,
  children,
}: {
  test: (session: ReturnType<typeof useSession>) => boolean;
  children: ReactNode;
}) {
  const session = useSession();

  if (!session.loaded) return null;
  if (!test(session)) {
    return <Alert severity="warning">You don't have permission to view this page.</Alert>;
  }

  return <>{children}</>;
}
