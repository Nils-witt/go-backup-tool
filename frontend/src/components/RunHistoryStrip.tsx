import { useState } from "react";
import type { JobRunEventJSON } from "../api/types";
import { fmtDuration, fmtSize, fmtTime } from "../lib/format";
import Box from "@mui/material/Box";
import Popover from "@mui/material/Popover";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

const MAX_RUNS = 20;
const HEIGHT = 28;

function runMs(r: JobRunEventJSON): number {
  return Math.max(0, new Date(r.end).getTime() - new Date(r.start).getTime());
}

// RunHistoryStrip draws a job's most recent runs as a small bar chart,
// oldest on the left: bar height is the run's duration (relative to the
// longest shown), color is its outcome. runs is newest first, as
// /api/job-runs serves it.
export function RunHistoryStrip({ runs }: { runs: JobRunEventJSON[] }) {
  const theme = useTheme();
  const [errorAt, setErrorAt] = useState<{ anchor: HTMLElement; error: string } | null>(null);
  const shown = runs.slice(0, MAX_RUNS).reverse();

  if (!shown.length) {
    return (
      <Typography variant="caption" color="text.secondary">
        no recorded runs yet
      </Typography>
    );
  }

  const maxMs = Math.max(...shown.map(runMs), 1);
  const ok = shown.filter((r) => r.success).length;
  const rate = Math.round((ok / shown.length) * 100);

  return (
    <Box>
      <Stack direction="row" sx={{ justifyContent: "space-between", mb: 0.5 }}>
        <Typography variant="caption" color="text.secondary">
          Last {shown.length} run{shown.length === 1 ? "" : "s"}
        </Typography>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ fontVariantNumeric: "tabular-nums" }}
        >
          {rate}% successful
        </Typography>
      </Stack>
      <Stack
        direction="row"
        spacing="2px"
        role="img"
        aria-label={`${ok} of ${shown.length} recent runs successful`}
        sx={{ height: HEIGHT, alignItems: "flex-end" }}
      >
        {shown.map((r, i) => {
          const ms = runMs(r);
          const h = Math.max(4, Math.round((ms / maxMs) * HEIGHT));

          return (
            <Tooltip
              key={i}
              arrow
              title={
                <>
                  <strong>{r.success ? "ok" : "failed"}</strong> · {fmtTime(r.end)}
                  <br />
                  took {fmtDuration(ms)}
                  {r.size ? " · " + fmtSize(r.size) : ""}
                  {r.error ? (
                    <>
                      <br />
                      click for error details
                    </>
                  ) : null}
                </>
              }
            >
              {/* The hit target spans the full strip height, not just the bar. */}
              <Box
                sx={{
                  flex: 1,
                  maxWidth: 14,
                  height: "100%",
                  display: "flex",
                  alignItems: "flex-end",
                  cursor: r.error ? "pointer" : "default",
                }}
                onClick={
                  r.error
                    ? (e) => setErrorAt({ anchor: e.currentTarget, error: r.error })
                    : undefined
                }
              >
                <Box
                  sx={{
                    width: "100%",
                    height: h,
                    borderRadius: "4px 4px 0 0",
                    bgcolor: r.success ? theme.palette.status.ok : theme.palette.status.failed,
                  }}
                />
              </Box>
            </Tooltip>
          );
        })}
      </Stack>
      <Popover
        open={errorAt !== null}
        anchorEl={errorAt?.anchor}
        onClose={() => setErrorAt(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "center" }}
        transformOrigin={{ vertical: "top", horizontal: "center" }}
      >
        <Typography
          variant="body2"
          color="error"
          sx={{ p: 1.5, maxWidth: 400, overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}
        >
          {errorAt?.error}
        </Typography>
      </Popover>
    </Box>
  );
}
