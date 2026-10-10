import { useMemo } from "react";
import { BrowserRouter } from "react-router-dom";
import CssBaseline from "@mui/material/CssBaseline";
import { ThemeProvider } from "@mui/material/styles";
import useMediaQuery from "@mui/material/useMediaQuery";
import { App } from "./App";
import { buildTheme } from "./theme";

// Root applies the light/dark theme (following the OS preference) and the
// router around App. Kept out of main.tsx so the entry module defines no
// component of its own (React fast refresh needs component-only modules).
export function Root() {
  const prefersDark = useMediaQuery("(prefers-color-scheme: dark)");
  const theme = useMemo(() => buildTheme(prefersDark ? "dark" : "light"), [prefersDark]);

  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ThemeProvider>
  );
}
