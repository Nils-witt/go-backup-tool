import { useState, type ReactNode } from "react";
import { apiFetch } from "../api/client";
import type { JobRunEventJSON, JobSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { RunHistoryStrip } from "./RunHistoryStrip";
import { fmtRelative, fmtTime, hasTime } from "../lib/format";
import { sourcedKey, type Sourced } from "../lib/status";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Grid from "@mui/material/Grid";
import LinearProgress from "@mui/material/LinearProgress";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

interface JobsGridProps {
  // jobs may come from several instances (see Sourced); remote ones never
  // offer a retry or show run history, which their API token can't reach.
  jobs: Sourced<JobSnapshot>[];
  canRetry: boolean;
  refreshNow: () => void;
  // runsByJob holds each job's recent runs, newest first; undefined hides
  // the run history (the viewer lacks the job run log permission).
  runsByJob?: Map<string, JobRunEventJSON[]>;
}

function Fact({ label, children, title }: { label: string; children: ReactNode; title?: string }) {
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

export function JobsGrid({ jobs, canRetry, refreshNow, runsByJob }: JobsGridProps) {
  const theme = useTheme();
  const [pendingRetry, setPendingRetry] = useState<string | null>(null);

  function startRetry(name: string) {
    apiFetch("/api/jobs/" + encodeURIComponent(name) + "/retry", { method: "POST" })
      .then(() => refreshNow())
      .catch(() => {});
  }

  if (!jobs.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no jobs configured
      </Typography>
    );
  }

  return (
    <>
      <Grid container spacing={2}>
        {jobs.map((j) => {
          const targets = j.targets || [];
          const hasFailedTarget = targets.some((t) => t.state === "failed");
          const accent = theme.palette.status[j.state] ?? theme.palette.status.idle;

          return (
            <Grid key={sourcedKey(j.source, j.name)} size={{ xs: 12, sm: 6, md: 4 }}>
              <Card
                variant="outlined"
                sx={{
                  height: "100%",
                  position: "relative",
                  borderLeft: 4,
                  borderLeftColor: accent,
                }}
              >
                {j.state === "running" ? (
                  <LinearProgress
                    color="inherit"
                    sx={{
                      position: "absolute",
                      top: 0,
                      left: 0,
                      right: 0,
                      height: 2,
                      color: accent,
                    }}
                  />
                ) : null}
                <CardContent>
                  <Stack
                    direction="row"
                    spacing={1}
                    sx={{ alignItems: "center", justifyContent: "space-between", mb: 1.5 }}
                  >
                    <Box sx={{ minWidth: 0 }}>
                      <Typography sx={{ fontWeight: 600, overflowWrap: "anywhere" }}>
                        {j.name}
                      </Typography>
                      {j.source ? (
                        <Typography variant="caption" color="text.secondary">
                          {j.source}
                        </Typography>
                      ) : null}
                    </Box>
                    <StatusChip state={j.state} error={j.error} />
                  </Stack>

                  <Box
                    sx={{
                      display: "grid",
                      gridTemplateColumns: "repeat(auto-fill, minmax(96px, 1fr))",
                      gap: 1,
                      mb: 1.5,
                    }}
                  >
                    <Fact label="Schedule">{j.interval ? "every " + j.interval : "once"}</Fact>
                    <Fact label="Last run" title={fmtTime(j.last_start)}>
                      {fmtRelative(j.last_start)}
                    </Fact>
                    <Fact
                      label="Next run"
                      title={hasTime(j.next_run) ? fmtTime(j.next_run) : undefined}
                    >
                      {hasTime(j.next_run) ? fmtRelative(j.next_run) : "—"}
                    </Fact>
                    {j.duration ? <Fact label="Took">{j.duration}</Fact> : null}
                    {j.size ? <Fact label="Size">{j.size}</Fact> : null}
                  </Box>

                  {runsByJob && !j.remote ? (
                    <Box sx={{ mb: 1.5 }}>
                      <RunHistoryStrip runs={runsByJob.get(j.name) ?? []} />
                    </Box>
                  ) : null}

                  <Typography variant="caption" color="text.secondary">
                    Targets
                  </Typography>
                  <Stack spacing={0.5} sx={{ mt: 0.5 }}>
                    {targets.map((t, i) => (
                      <Stack key={i} direction="row" spacing={1} sx={{ alignItems: "center" }}>
                        <Typography variant="body2" sx={{ flex: 1, overflowWrap: "anywhere" }}>
                          {t.server} / {t.bucket}{" "}
                          <Typography component="span" variant="caption" color="text.secondary">
                            ({t.kind})
                          </Typography>
                        </Typography>
                        <StatusChip state={t.state} error={t.error} />
                      </Stack>
                    ))}
                  </Stack>

                  {hasFailedTarget && canRetry && !j.remote ? (
                    <Box sx={{ mt: 1.5 }}>
                      <Button
                        size="small"
                        variant="outlined"
                        onClick={() => setPendingRetry(j.name)}
                      >
                        Retry failed targets
                      </Button>
                    </Box>
                  ) : null}
                </CardContent>
              </Card>
            </Grid>
          );
        })}
      </Grid>

      <ConfirmDialog
        open={pendingRetry !== null}
        message={
          <>
            Retry failed targets for <strong>{pendingRetry}</strong>?
          </>
        }
        confirmLabel="Retry"
        onConfirm={() => {
          if (pendingRetry) startRetry(pendingRetry);
          setPendingRetry(null);
        }}
        onCancel={() => setPendingRetry(null)}
      />
    </>
  );
}
