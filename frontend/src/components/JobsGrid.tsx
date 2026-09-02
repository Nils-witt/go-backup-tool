import { useState } from "react";
import { apiFetch } from "../api/client";
import type { JobSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { fmtTime, hasTime } from "../lib/format";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Grid from "@mui/material/Grid";
import List from "@mui/material/List";
import ListItem from "@mui/material/ListItem";
import ListItemText from "@mui/material/ListItemText";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";

interface JobsGridProps {
  jobs: JobSnapshot[];
  canRetry: boolean;
  refreshNow: () => void;
}

export function JobsGrid({ jobs, canRetry, refreshNow }: JobsGridProps) {
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
          const hasFailedTarget = (j.targets || []).some((t) => t.state === "failed");
          const interval = j.interval ? "every " + j.interval : "runs once";
          const duration = j.duration ? " · took " + j.duration : "";
          const size = j.size ? " · " + j.size : "";
          const nextRun = hasTime(j.next_run) ? " · next run: " + fmtTime(j.next_run) : "";

          return (
            <Grid key={j.name} size={{ xs: 12, sm: 6, md: 4 }}>
              <Card variant="outlined" sx={{ height: "100%" }}>
                <CardContent>
                  <Stack
                    direction="row"
                    spacing={1}
                    sx={{ alignItems: "baseline", justifyContent: "space-between", mb: 0.5 }}
                  >
                    <Typography sx={{ fontWeight: 600 }}>{j.name}</Typography>
                    <StatusChip state={j.state} />
                  </Stack>
                  <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
                    {interval} · last run: {fmtTime(j.last_start)}
                    {duration}
                    {size}
                    {nextRun}
                  </Typography>
                  {j.error ? (
                    <Typography variant="body2" color="error" sx={{ overflowWrap: "anywhere" }}>
                      {j.error}
                    </Typography>
                  ) : null}
                  <List dense disablePadding sx={{ borderTop: 1, borderColor: "divider" }}>
                    {(j.targets || []).map((t, i) => (
                      <ListItem
                        key={i}
                        disableGutters
                        divider
                        secondaryAction={<StatusChip state={t.state} />}
                        sx={{ py: 0.75 }}
                      >
                        <ListItemText
                          slotProps={{
                            primary: { variant: "body2", sx: { overflowWrap: "anywhere" } },
                          }}
                          primary={
                            <>
                              {t.server} / {t.bucket}{" "}
                              <Typography component="span" variant="caption" color="text.secondary">
                                ({t.kind})
                              </Typography>
                            </>
                          }
                          secondary={
                            t.error ? (
                              <Typography
                                variant="caption"
                                color="error"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                {t.error}
                              </Typography>
                            ) : null
                          }
                        />
                      </ListItem>
                    ))}
                  </List>
                  {hasFailedTarget && canRetry ? (
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
