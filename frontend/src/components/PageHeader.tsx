import type { ReactNode } from "react";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";

export function PageHeader({
  title,
  subtitle,
  action,
}: {
  title: string;
  subtitle?: ReactNode;
  // action is shown at the header's right edge, e.g. a page-level button.
  action?: ReactNode;
}) {
  return (
    <Stack
      direction="row"
      spacing={2}
      sx={{ mb: 3, alignItems: "flex-start", justifyContent: "space-between" }}
    >
      <Stack spacing={0.5}>
        <Typography variant="h5" sx={{ fontWeight: 600 }}>
          {title}
        </Typography>
        {subtitle ? (
          <Typography variant="body2" color="text.secondary">
            {subtitle}
          </Typography>
        ) : null}
      </Stack>
      {action}
    </Stack>
  );
}
