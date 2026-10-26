import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Root } from "./Root";

// The dashboard no longer fetches remote backends itself (its
// Content-Security-Policy only lets it connect to this origin and the SSO
// provider; other instances push their status to this one instead), so drop
// the list earlier versions stored for them, which held each remote's API
// token.
try {
  localStorage.removeItem("gbt.remoteBackends");
} catch {
  // Storage unavailable: nothing was stored either.
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
