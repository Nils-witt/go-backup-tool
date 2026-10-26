import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Root } from "./Root";

// The dashboard no longer merges in remote backends (its
// Content-Security-Policy only lets it connect to this origin and the SSO
// provider), so drop what earlier versions stored for them: the list held
// each remote's API token.
try {
  localStorage.removeItem("gbt.remoteBackends");
  localStorage.removeItem("gbt.hiddenInstances");
} catch {
  // Storage unavailable: nothing was stored either.
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
