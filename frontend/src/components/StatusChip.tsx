import { useState } from "react";
import Chip from "@mui/material/Chip";
import ErrorOutlineIcon from "@mui/icons-material/ErrorOutlineOutlined";
import Popover from "@mui/material/Popover";
import Typography from "@mui/material/Typography";
import { alpha, useTheme } from "@mui/material/styles";

const STATES = ["ok", "failed", "running", "incomplete", "idle"] as const;
type KnownState = (typeof STATES)[number];

// StatusChip shows a run state. Given an error, the chip gains an error
// icon and becomes clickable, revealing the error text in a popover rather
// than inline.
export function StatusChip({
  state,
  label,
  error,
}: {
  state: string;
  label?: string;
  error?: string;
}) {
  const theme = useTheme();
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const color =
    theme.palette.status[
      (STATES as readonly string[]).includes(state) ? (state as KnownState) : "idle"
    ];

  return (
    <>
      <Chip
        size="small"
        label={label ?? state}
        icon={error ? <ErrorOutlineIcon /> : undefined}
        onClick={error ? (e) => setAnchor(e.currentTarget) : undefined}
        aria-label={error ? `${label ?? state}: show error details` : undefined}
        sx={{
          color,
          backgroundColor: alpha(color, theme.palette.mode === "dark" ? 0.18 : 0.12),
          fontWeight: 600,
          fontSize: ".72rem",
          textTransform: "uppercase",
          letterSpacing: ".02em",
          "& .MuiChip-icon": { color, fontSize: "1rem" },
          "&.MuiChip-clickable:hover": {
            backgroundColor: alpha(color, theme.palette.mode === "dark" ? 0.28 : 0.2),
          },
        }}
      />
      {error ? (
        <Popover
          open={anchor !== null}
          anchorEl={anchor}
          onClose={() => setAnchor(null)}
          anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
          transformOrigin={{ vertical: "top", horizontal: "right" }}
        >
          <Typography
            variant="body2"
            color="error"
            sx={{ p: 1.5, maxWidth: 400, overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}
          >
            {error}
          </Typography>
        </Popover>
      ) : null}
    </>
  );
}
