import { createTheme, type PaletteMode } from "@mui/material/styles";

// Status colors mirror the pre-MUI --ok/--failed/--running/--incomplete/--idle
// custom properties from the ported dashboard.html stylesheet, kept as a
// second palette namespace (theme.status.*) rather than shoehorned into MUI's
// own success/error/warning slots, since "incomplete" and "idle" don't map
// onto those cleanly.
declare module "@mui/material/styles" {
  interface Palette {
    status: {
      ok: string;
      failed: string;
      running: string;
      incomplete: string;
      idle: string;
    };
  }
  interface PaletteOptions {
    status?: {
      ok: string;
      failed: string;
      running: string;
      incomplete: string;
      idle: string;
    };
  }
}

export function buildTheme(mode: PaletteMode) {
  return createTheme({
    palette: {
      mode,
      status:
        mode === "dark"
          ? {
              ok: "#56d364",
              failed: "#ff7b72",
              running: "#e3b341",
              incomplete: "#ffa657",
              idle: "#9a9aa0",
            }
          : {
              ok: "#1a7f37",
              failed: "#b3261e",
              running: "#9a6700",
              incomplete: "#bf5b04",
              idle: "#6b6b70",
            },
    },
    shape: { borderRadius: 8 },
    typography: {
      fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
    },
    components: {
      MuiContainer: {
        defaultProps: { maxWidth: "lg" },
      },
    },
  });
}
