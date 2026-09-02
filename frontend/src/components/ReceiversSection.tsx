import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { ReceiverFile, ReceiverSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { ConfirmDialog } from "./ConfirmDialog";
import { encodePathKey, fmtSize, fmtTime, hasTime } from "../lib/format";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Collapse from "@mui/material/Collapse";
import Grid from "@mui/material/Grid";
import Link from "@mui/material/Link";
import List from "@mui/material/List";
import ListItem from "@mui/material/ListItem";
import ListItemText from "@mui/material/ListItemText";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";

interface ReceiversSectionProps {
  receivers: ReceiverSnapshot[];
  canDownload: boolean;
}

function startDownload(id: string, key: string) {
  const url = "/api/receivers/" + encodeURIComponent(id) + "/download/" + encodePathKey(key);

  apiFetch(url, { method: "POST" })
    .then((r) => r.json())
    .then((data: { ticket: string }) => {
      window.location.href = url + "?ticket=" + encodeURIComponent(data.ticket);
    })
    .catch(() => {});
}

function FileList({
  id,
  canDownload,
  onDownload,
}: {
  id: string;
  canDownload: boolean;
  onDownload: (id: string, key: string) => void;
}) {
  const [files, setFiles] = useState<ReceiverFile[] | null>(null);

  useEffect(() => {
    let cancelled = false;

    apiFetchJSON<ReceiverFile[]>("/api/receivers/" + encodeURIComponent(id) + "/files")
      .then((f) => {
        if (!cancelled) setFiles(f || []);
      })
      .catch(() => {
        if (!cancelled) setFiles([]);
      });

    return () => {
      cancelled = true;
    };
  }, [id]);

  if (files === null)
    return (
      <Typography variant="body2" color="text.secondary">
        loading…
      </Typography>
    );
  if (!files.length)
    return (
      <Typography variant="body2" color="text.secondary">
        no files stored
      </Typography>
    );

  return (
    <List
      dense
      disablePadding
      sx={{ maxHeight: 220, overflowY: "auto", borderTop: 1, borderColor: "divider" }}
    >
      {files.map((f) => (
        <ListItem key={f.key} disableGutters divider sx={{ py: 0.5 }}>
          <ListItemText
            slotProps={{
              primary: { variant: "body2", sx: { overflowWrap: "anywhere" } },
              secondary: { variant: "caption" },
            }}
            primary={f.key}
            secondary={
              <>
                {fmtSize(f.size)} · {fmtTime(f.mod_time)}
                {canDownload ? (
                  <>
                    {" · "}
                    <Link
                      component="button"
                      variant="caption"
                      underline="hover"
                      onClick={() => onDownload(id, f.key)}
                    >
                      download
                    </Link>
                  </>
                ) : null}
              </>
            }
          />
        </ListItem>
      ))}
    </List>
  );
}

export function ReceiversSection({ receivers, canDownload }: ReceiversSectionProps) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [everExpanded, setEverExpanded] = useState<Record<string, boolean>>({});
  const [pendingDownload, setPendingDownload] = useState<{ id: string; key: string } | null>(null);

  if (!receivers.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no receivers configured
      </Typography>
    );
  }

  return (
    <>
      <Grid container spacing={2}>
        {receivers.map((rcv) => {
          const retention = rcv.retention ? " · retention " + rcv.retention : "";
          const staleAfter = rcv.stale_after ? " · stale after " + rcv.stale_after : "";
          const lastSeen = hasTime(rcv.last_seen)
            ? "last received: " +
              fmtTime(rcv.last_seen) +
              (rcv.last_key ? " (" + rcv.last_key + ")" : "")
            : "no objects received yet";
          const isExpanded = !!expanded[rcv.id];

          return (
            <Grid key={rcv.id} size={{ xs: 12, sm: 6, md: 4 }}>
              <Card variant="outlined" sx={{ height: "100%" }}>
                <CardContent>
                  <Stack
                    direction="row"
                    spacing={1}
                    sx={{ alignItems: "baseline", justifyContent: "space-between", mb: 0.5 }}
                  >
                    <Typography sx={{ fontWeight: 600 }}>{rcv.id}</Typography>
                    <Stack direction="row" spacing={0.5}>
                      <StatusChip state={rcv.state} />
                      {rcv.stale ? <StatusChip state="failed" label="stale" /> : null}
                    </Stack>
                  </Stack>
                  <Typography variant="body2" color="text.secondary">
                    {rcv.path}
                    {retention}
                    {staleAfter}
                  </Typography>
                  <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
                    {lastSeen}
                  </Typography>
                  {rcv.error ? (
                    <Typography variant="body2" color="error" sx={{ overflowWrap: "anywhere" }}>
                      {rcv.error}
                    </Typography>
                  ) : null}
                  <Button
                    size="small"
                    variant="outlined"
                    onClick={() => {
                      setExpanded((prev) => ({ ...prev, [rcv.id]: !prev[rcv.id] }));
                      setEverExpanded((prev) =>
                        prev[rcv.id] ? prev : { ...prev, [rcv.id]: true },
                      );
                    }}
                  >
                    {isExpanded ? "Hide files" : "Show files"}
                  </Button>
                  <Collapse in={isExpanded}>
                    <Box sx={{ mt: 1.5 }}>
                      {everExpanded[rcv.id] ? (
                        <FileList
                          id={rcv.id}
                          canDownload={canDownload}
                          onDownload={(id, key) => setPendingDownload({ id, key })}
                        />
                      ) : null}
                    </Box>
                  </Collapse>
                </CardContent>
              </Card>
            </Grid>
          );
        })}
      </Grid>

      <ConfirmDialog
        open={pendingDownload !== null}
        message={
          <>
            Download <strong>{pendingDownload?.key}</strong>?
          </>
        }
        confirmLabel="Download"
        onConfirm={() => {
          if (pendingDownload) startDownload(pendingDownload.id, pendingDownload.key);
          setPendingDownload(null);
        }}
        onCancel={() => setPendingDownload(null)}
      />
    </>
  );
}
