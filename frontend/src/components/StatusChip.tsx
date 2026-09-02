import Chip from "@mui/material/Chip";
import { alpha, useTheme } from "@mui/material/styles";

const STATES = ["ok", "failed", "running", "incomplete", "idle"] as const;
type KnownState = (typeof STATES)[number];

export function StatusChip({ state, label }: { state: string; label?: string }) {
  const theme = useTheme();
  const color =
    theme.palette.status[
      (STATES as readonly string[]).includes(state) ? (state as KnownState) : "idle"
    ];

  return (
    <Chip
      size="small"
      label={label ?? state}
      sx={{
        color,
        backgroundColor: alpha(color, theme.palette.mode === "dark" ? 0.18 : 0.12),
        fontWeight: 600,
        fontSize: ".72rem",
        textTransform: "uppercase",
        letterSpacing: ".02em",
      }}
    />
  );
}
