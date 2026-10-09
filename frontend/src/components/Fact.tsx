import type { ReactNode } from "react";
import Box from "@mui/material/Box";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";

// Fact is one labelled value in a dashboard card's fact grid; given a
// title, hovering the value reveals it (e.g. the exact time behind "3h ago").
export function Fact({
  label,
  children,
  title,
}: {
  label: string;
  children: ReactNode;
  title?: string;
}) {
  const value = (
    <Typography variant="body2" sx={{ fontVariantNumeric: "tabular-nums" }}>
      {children}
    </Typography>
  );

  return (
    <Box>
      <Typography variant="caption" color="text.secondary">
        {label}
      </Typography>
      {title ? (
        <Tooltip title={title} arrow>
          {value}
        </Tooltip>
      ) : (
        value
      )}
    </Box>
  );
}
