import type { ReactNode } from "react";
import type { RunState } from "../api/types";
import type { StateCounts } from "../lib/status";
import Box from "@mui/material/Box";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

// The order segments are stacked and listed in: problems first, so they
// sit at the bar's left edge where the eye lands.
const ORDER: RunState[] = ["failed", "incomplete", "running", "ok", "idle"];

// StatusBar is a single stacked bar of counts per state, with a 2px gap
// between segments so adjacent states stay distinguishable.
export function StatusBar({
  counts,
  labels,
}: {
  counts: StateCounts;
  labels?: Partial<Record<RunState, string>>;
}) {
  const theme = useTheme();
  const total = ORDER.reduce((n, s) => n + (counts[s] ?? 0), 0);

  if (!total) {
    return <Box sx={{ height: 8, borderRadius: 4, bgcolor: "action.hover" }} />;
  }

  return (
    <Stack direction="row" spacing="2px" sx={{ height: 8 }}>
      {ORDER.filter((s) => counts[s]).map((s) => (
        <Tooltip key={s} title={`${counts[s]} ${labels?.[s] ?? s}`} arrow>
          <Box
            sx={{
              flex: counts[s],
              bgcolor: theme.palette.status[s],
              borderRadius: "4px",
              minWidth: 6,
            }}
          />
        </Tooltip>
      ))}
    </Stack>
  );
}

function Legend({
  counts,
  labels,
}: {
  counts: StateCounts;
  labels?: Partial<Record<RunState, string>>;
}) {
  const theme = useTheme();

  return (
    <Stack direction="row" sx={{ flexWrap: "wrap", columnGap: 1.5, rowGap: 0.5, mt: 1 }}>
      {ORDER.filter((s) => counts[s]).map((s) => (
        <Stack key={s} direction="row" spacing={0.75} sx={{ alignItems: "center" }}>
          <Box
            sx={{ width: 8, height: 8, borderRadius: "50%", bgcolor: theme.palette.status[s] }}
          />
          <Typography variant="caption" color="text.secondary">
            {counts[s]} {labels?.[s] ?? s}
          </Typography>
        </Stack>
      ))}
    </Stack>
  );
}

interface SummaryTileProps {
  title: string;
  value: ReactNode;
  caption?: ReactNode;
  counts?: StateCounts;
  labels?: Partial<Record<RunState, string>>;
}

export function SummaryTile({ title, value, caption, counts, labels }: SummaryTileProps) {
  return (
    <Card variant="outlined" sx={{ height: "100%" }}>
      <CardContent>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ textTransform: "uppercase", letterSpacing: ".04em", fontWeight: 600 }}
        >
          {title}
        </Typography>
        <Typography
          variant="h4"
          sx={{ fontWeight: 600, lineHeight: 1.2, my: 0.5, fontVariantNumeric: "tabular-nums" }}
        >
          {value}
        </Typography>
        {caption ? (
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ mb: counts ? 1.5 : 0, overflowWrap: "anywhere" }}
          >
            {caption}
          </Typography>
        ) : null}
        {counts ? (
          <>
            <StatusBar counts={counts} labels={labels} />
            <Legend counts={counts} labels={labels} />
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
