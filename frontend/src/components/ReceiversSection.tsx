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
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

interface ReceiversSectionProps {
  // receivers may come from several instances (see Sourced); remote ones
  // offer no file listing: their files are on the other instance.
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

// objectPath is f's path under its receiver, as the download endpoint
// addresses it: key/file for one file of a multi-file backup.
function objectPath(f: ReceiverFile): string {
  return f.file ? f.key + "/" + f.file : f.key;
}

// FileGroup is one backup in the listing: a flat object (one file, no
// file name), or a multi-file backup's files under their shared key — or,
// on a directory receiver, one file's versions under its path.
interface FileGroup {
  key: string;
  isMultiFile: boolean;
  isVersions?: boolean;
  files: ReceiverFile[];
}

// groupVersions groups a directory receiver's versions by path, in path
// order, each path's newest version first.
function groupVersions(files: ReceiverFile[]): FileGroup[] {
  const byPath = new Map<string, FileGroup>();

  for (const f of files) {
    const path = f.path ?? f.key;
    let g = byPath.get(path);
    if (!g) {
      g = { key: path, isMultiFile: true, isVersions: true, files: [] };
      byPath.set(path, g);
    }
    g.files.push(f);
  }

  const groups = [...byPath.values()].sort((a, b) => a.key.localeCompare(b.key));
  for (const g of groups) g.files.sort((a, b) => (b.version ?? 0) - (a.version ?? 0));
  return groups;
}

// groupFiles groups files by backup, keeping the listing's (oldest-first)
// order by each backup's first file.
function groupFiles(files: ReceiverFile[]): FileGroup[] {
  if (files.some((f) => f.version)) return groupVersions(files);

  const groups: FileGroup[] = [];
  const byKey = new Map<string, FileGroup>();

  for (const f of files) {
    if (!f.file) {
      groups.push({ key: f.key, isMultiFile: false, files: [f] });
      continue;
    }

    let g = byKey.get(f.key);
    if (!g) {
      g = { key: f.key, isMultiFile: true, files: [] };
      byKey.set(f.key, g);
      groups.push(g);
    }

    g.files.push(f);
  }

  return groups;
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
              {groupFiles(files).map((g) => [
                g.isMultiFile ? (
                  <TableRow key={"backup:" + g.key} sx={{ bgcolor: "action.hover" }}>
                    <TableCell sx={{ overflowWrap: "anywhere", fontWeight: 500 }}>
                      {g.key}
                    </TableCell>
                    <TableCell>{fmtSize(g.files.reduce((n, f) => n + f.size, 0))}</TableCell>
                    <TableCell colSpan={3}>
                      <Typography variant="caption" color="text.secondary">
                        {g.files.length}{" "}
                        {g.isVersions
                          ? g.files.length === 1
                            ? "version"
                            : "versions"
                          : g.files.length === 1
                            ? "file"
                            : "files"}
                      </Typography>
                    </TableCell>
                  </TableRow>
                ) : null,
                ...g.files.map((f) => (
                  <TableRow key={objectPath(f)}>
                    <TableCell sx={{ overflowWrap: "anywhere", pl: g.isMultiFile ? 4 : undefined }}>
                      {g.isVersions ? (
                        <Tooltip title={f.sha256 ? "SHA-256 of the content: " + f.sha256 : ""}>
                          <span>
                            version {f.version}
                            {f === g.files[0] ? " (newest)" : ""}
                          </span>
                        </Tooltip>
                      ) : (
                        (f.file ?? f.key)
                      )}
                    </TableCell>
                    <TableCell>{fmtSize(f.size)}</TableCell>
                    <TableCell>{fmtTime(f.mod_time)}</TableCell>
                    <TableCell>{fmtTime(f.expires_at)}</TableCell>
                    <TableCell>
                      {canDownload && id ? (
                        <Link
                          component="button"
                          variant="body2"
                          underline="hover"
                          onClick={() => onDownload(id, objectPath(f))}
                        >
                          download
                        </Link>
                      ) : null}
                    </TableCell>
                  </TableRow>
                )),
              ])}
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
              {receiver.instance ? (
                <Typography variant="caption" color="text.secondary">
                  {receiver.instance}
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
            {receiver.kind === "directory" ? <Fact label="Type">directory</Fact> : null}
            <Fact label="Retention">{receiver.retention || "—"}</Fact>
            {receiver.keep_versions ? (
              <Fact label="Keep versions">{receiver.keep_versions}</Fact>
            ) : null}
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
            <Grid key={sourcedKey(rcv.instance, rcv.id)} size={{ xs: 12, sm: 6, md: 4 }}>
              <ReceiversCard receiver={rcv} canDownload={canDownload} />
            </Grid>
          );
        })}
      </Grid>
    </>
  );
}
