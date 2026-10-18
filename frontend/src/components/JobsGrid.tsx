import { useState } from "react";
import { apiFetch } from "../api/client";
import type { JobRunEventJSON, JobSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { Fact } from "./Fact";
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
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

// JobAction is a job card's admin action: "run" starts a run right away,
// "retry" re-runs just the failed targets. Each is also the path segment of
// its endpoint, POST /api/jobs/{name}/{action}.
type JobAction = "run" | "retry";

const jobActionPrompts: Record<JobAction, { message: string; confirmLabel: string }> = {
  run: { message: "Run job", confirmLabel: "Run now" },
  retry: { message: "Retry failed targets for", confirmLabel: "Retry" },
};

// maxOnChangeFiles is how many of an on-change job's last-run files its
// card lists.
const maxOnChangeFiles = 5;

// scheduleText describes when j runs.
function scheduleText(j: JobSnapshot): string {
  if (j.kind === "on-change") {
    const parts = [j.watching ? "on change" : "", j.interval ? "scan every " + j.interval : ""];
    return parts.filter(Boolean).join(", ") || "once";
  }
  return j.interval ? "every " + j.interval : "once";
}

// shownFiles is the part of j's files its card lists.
function shownFiles(j: JobSnapshot) {
  const files = j.files ?? [];
  return j.kind === "on-change" ? files.slice(0, maxOnChangeFiles) : files;
}

interface JobsGridProps {
  // jobs may come from several instances (see Sourced); remote ones never
  // offer a run or retry, which their API token can't reach.
  jobs: Sourced<JobSnapshot>[];
  canRetry: boolean;
  refreshNow: () => void;
  // runsByJob holds each job's recent runs, newest first, keyed by
  // sourcedKey(source, name); a job without an entry shows no run history
  // (its instance's job run log isn't readable).
  runsByJob: Map<string, JobRunEventJSON[]>;
}

export function JobsGrid({ jobs, canRetry, refreshNow, runsByJob }: JobsGridProps) {
  const theme = useTheme();
  const [pending, setPending] = useState<{ name: string; action: JobAction } | null>(null);

  function startAction(name: string, action: JobAction) {
    apiFetch("/api/jobs/" + encodeURIComponent(name) + "/" + action, { method: "POST" })
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
          const runs = runsByJob.get(sourcedKey(j.source, j.name));

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
                    <Fact label="Schedule">{scheduleText(j)}</Fact>
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

                  {runs ? (
                    <Box sx={{ mb: 1.5 }}>
                      <RunHistoryStrip runs={runs} />
                    </Box>
                  ) : null}

                  {j.kind === "on-change" && j.source ? (
                    <>
                      <Typography variant="caption" color="text.secondary">
                        Source
                      </Typography>
                      <Typography
                        variant="body2"
                        sx={{ mt: 0.5, mb: 1.5, overflowWrap: "anywhere" }}
                      >
                        {j.source}
                      </Typography>
                    </>
                  ) : null}

                  {j.files?.length ? (
                    <>
                      <Typography variant="caption" color="text.secondary">
                        {j.kind === "on-change" ? "Last run's files" : "Files"}
                      </Typography>
                      <Stack spacing={0.5} sx={{ mt: 0.5, mb: 1.5 }}>
                        {shownFiles(j).map((f) => (
                          <Stack
                            key={f.name}
                            direction="row"
                            spacing={1}
                            sx={{ alignItems: "center" }}
                          >
                            <Typography variant="body2" sx={{ flex: 1, overflowWrap: "anywhere" }}>
                              {f.name}{" "}
                              <Typography component="span" variant="caption" color="text.secondary">
                                ({f.command}
                                {f.size ? ", " + f.size : ""})
                              </Typography>
                            </Typography>
                            <StatusChip state={f.state} error={f.error} />
                          </Stack>
                        ))}
                        {j.files.length > shownFiles(j).length ? (
                          <Typography variant="caption" color="text.secondary">
                            and {j.files.length - shownFiles(j).length} more
                          </Typography>
                        ) : null}
                      </Stack>
                    </>
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

                  {canRetry && !j.remote && j.state !== "running" ? (
                    <Stack direction="row" spacing={1} sx={{ mt: 1.5, flexWrap: "wrap" }}>
                      <Button
                        size="small"
                        variant="outlined"
                        onClick={() => setPending({ name: j.name, action: "run" })}
                      >
                        Run now
                      </Button>
                      {hasFailedTarget ? (
                        <Button
                          size="small"
                          variant="outlined"
                          onClick={() => setPending({ name: j.name, action: "retry" })}
                        >
                          Retry failed targets
                        </Button>
                      ) : null}
                    </Stack>
                  ) : null}
                </CardContent>
              </Card>
            </Grid>
          );
        })}
      </Grid>

      <ConfirmDialog
        open={pending !== null}
        message={
          pending ? (
            <>
              {jobActionPrompts[pending.action].message} <strong>{pending.name}</strong>?
            </>
          ) : null
        }
        confirmLabel={pending ? jobActionPrompts[pending.action].confirmLabel : ""}
        onConfirm={() => {
          if (pending) startAction(pending.name, pending.action);
          setPending(null);
        }}
        onCancel={() => setPending(null)}
      />
    </>
  );
}
