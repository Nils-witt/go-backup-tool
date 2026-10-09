import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { ReceiverFile, ReceiverSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { Fact } from "./Fact";
import { ConfirmDialog } from "./ConfirmDialog";
import { encodePathKey, fmtRelative, fmtSize, fmtTime, hasTime } from "../lib/format";
import { sourcedKey, type Sourced } from "../lib/status";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import CloseIcon from "@mui/icons-material/Close";
import Dialog from "@mui/material/Dialog";
import Grid from "@mui/material/Grid";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

interface ReceiversSectionProps {
  // receivers may come from several instances (see Sourced); remote ones
  // offer no file listing, since their downloads can't work cross-origin.
  receivers: Sourced<ReceiverSnapshot>[];
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

function FileListDialog({
  id,
  canDownload,
  onClose,
  onDownload,
}: {
  id: string | null;
  canDownload: boolean;
  onClose: () => void;
  onDownload: (id: string, key: string) => void;
}) {
  // The listing is stored with the receiver id it belongs to, so switching
  // to another receiver shows "loading…" (files === null) until its own
  // listing arrives, without resetting state inside the effect.
  const [loaded, setLoaded] = useState<{ id: string; files: ReceiverFile[] } | null>(null);
  const files = loaded && loaded.id === id ? loaded.files : null;

  useEffect(() => {
    if (!id) return;

    let cancelled = false;

    apiFetchJSON<ReceiverFile[]>("/api/receivers/" + encodeURIComponent(id) + "/files")
      .then((f) => {
        if (!cancelled) setLoaded({ id, files: f || [] });
      })
      .catch(() => {
        if (!cancelled) setLoaded({ id, files: [] });
      });

    return () => {
      cancelled = true;
    };
  }, [id]);

  return (
    <Dialog open={id !== null} onClose={onClose} fullScreen>
      <AppBar position="relative" color="default" elevation={1}>
        <Toolbar sx={{ gap: 1 }}>
          <Typography variant="h6" component="div" sx={{ flexGrow: 1 }}>
            Files{id ? " · " + id : ""}
          </Typography>
          <IconButton edge="end" onClick={onClose} aria-label="close">
            <CloseIcon />
          </IconButton>
        </Toolbar>
      </AppBar>
      <Box sx={{ p: 2 }}>
        {files === null ? (
          <Typography color="text.secondary">loading…</Typography>
        ) : !files.length ? (
          <Typography color="text.secondary">no files stored</Typography>
        ) : (
          <Table size="small">
            <TableHead>
              <TableRow>
                <TableCell>Key</TableCell>
                <TableCell>Size</TableCell>
                <TableCell>Modified</TableCell>
                <TableCell>Expires</TableCell>
                <TableCell />
              </TableRow>
            </TableHead>
            <TableBody>
              {files.map((f) => (
                <TableRow key={f.key}>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>{f.key}</TableCell>
                  <TableCell>{fmtSize(f.size)}</TableCell>
                  <TableCell>{fmtTime(f.mod_time)}</TableCell>
                  <TableCell>{fmtTime(f.expires_at)}</TableCell>
                  <TableCell>
                    {canDownload && id ? (
                      <Link
                        component="button"
                        variant="body2"
                        underline="hover"
                        onClick={() => onDownload(id, f.key)}
                      >
                        download
                      </Link>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Box>
    </Dialog>
  );
}

export function ReceiversCard({
  receiver,
  canDownload,
}: {
  receiver: Sourced<ReceiverSnapshot>;
  canDownload: boolean;
}) {
  const theme = useTheme();
  // A stale receiver is accented like an incomplete run, matching how the
  // summary bar counts it.
  const accent = theme.palette.status[receiver.stale ? "incomplete" : receiver.state];

  const [openFilesFor, setOpenFilesFor] = useState<string | null>(null);
  const [pendingDownload, setPendingDownload] = useState<{ id: string; key: string } | null>(null);

  return (
    <>
      <Card
        variant="outlined"
        sx={{ height: "100%", borderLeft: 4, borderLeftColor: accent ?? theme.palette.status.idle }}
      >
        <CardContent>
          <Stack
            direction="row"
            spacing={1}
            sx={{ alignItems: "center", justifyContent: "space-between", mb: 1.5 }}
          >
            <Box sx={{ minWidth: 0 }}>
              <Typography sx={{ fontWeight: 600, overflowWrap: "anywhere" }}>
                {receiver.id}
              </Typography>
              {receiver.source ? (
                <Typography variant="caption" color="text.secondary">
                  {receiver.source}
                </Typography>
              ) : null}
            </Box>
            <Stack direction="row" spacing={0.5}>
              {receiver.stale ? <StatusChip state="incomplete" label="stale" /> : null}
              <StatusChip state={receiver.state} error={receiver.error} />
            </Stack>
          </Stack>

          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fill, minmax(96px, 1fr))",
              gap: 1,
              mb: 1.5,
            }}
          >
            <Fact
              label="Last received"
              title={hasTime(receiver.last_seen) ? fmtTime(receiver.last_seen) : undefined}
            >
              {fmtRelative(receiver.last_seen)}
            </Fact>
            <Fact label="Retention">{receiver.retention || "—"}</Fact>
            {receiver.stale_after ? <Fact label="Stale after">{receiver.stale_after}</Fact> : null}
          </Box>

          <Typography variant="caption" color="text.secondary">
            Path
          </Typography>
          <Typography variant="body2" sx={{ mt: 0.5, overflowWrap: "anywhere" }}>
            {receiver.path}
          </Typography>

          {receiver.last_key ? (
            <>
              <Typography variant="caption" color="text.secondary" component="div" sx={{ mt: 1 }}>
                Last object
              </Typography>
              <Typography variant="body2" sx={{ mt: 0.5, overflowWrap: "anywhere" }}>
                {receiver.last_key}
              </Typography>
            </>
          ) : null}

          {receiver.remote ? null : (
            <Box sx={{ mt: 1.5 }}>
              <Button size="small" variant="outlined" onClick={() => setOpenFilesFor(receiver.id)}>
                Show files
              </Button>
            </Box>
          )}
        </CardContent>
      </Card>
      <FileListDialog
        id={openFilesFor}
        canDownload={canDownload}
        onClose={() => setOpenFilesFor(null)}
        onDownload={(id, key) => setPendingDownload({ id, key })}
      />
      <ConfirmDialog
        open={pendingDownload !== null}
        message={
          <>
            Download <strong>{pendingDownload?.key}</strong>? Downloading may trigger notifications
            and other hooks to record and inform about the download. Make sure you have permission
            and cause to download this file.
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

export function ReceiversSection({ receivers, canDownload }: ReceiversSectionProps) {
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
          return (
            <Grid key={sourcedKey(rcv.source, rcv.id)} size={{ xs: 12, sm: 6, md: 4 }}>
              <ReceiversCard receiver={rcv} canDownload={canDownload} />
            </Grid>
          );
        })}
      </Grid>
    </>
  );
}
