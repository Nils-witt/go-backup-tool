import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { GPGKeyJSON, GPGKeyListJSON, ReceiverFile } from "../api/types";
import { groupFingerprint, keyEmails, keyForKeyID } from "../lib/gpg";
import { ConfirmDialog } from "./ConfirmDialog";
import { encodePathKey, fmtSize, fmtTime } from "../lib/format";
import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";

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

// HIDDEN_KEY_ID is the wildcard key ID of a recipient the sender hid (gpg
// --throw-keyids); see pgprecipients.HiddenKeyID.
const HIDDEN_KEY_ID = "0000000000000000";

// RecipientKeyIDs lists the key IDs a file is encrypted to, one per line,
// each with its key's email addresses when keys (the keyring, null when it
// can't be listed) holds it.
function RecipientKeyIDs({ ids, keys }: { ids?: string[]; keys: GPGKeyJSON[] | null }) {
  if (!ids?.length) {
    return (
      <Typography variant="caption" color="text.secondary">
        —
      </Typography>
    );
  }

  return ids.map((id, i) => {
    if (id === HIDDEN_KEY_ID) {
      return (
        <Tooltip key={i} title="The sender hid this recipient's key ID (gpg --throw-keyids).">
          <Typography variant="caption" color="text.secondary" sx={{ display: "block" }}>
            hidden recipient
          </Typography>
        </Tooltip>
      );
    }

    const k = keys ? keyForKeyID(keys, id) : undefined;
    const emails = k ? keyEmails(k).join(", ") : "";
    const title = k
      ? `Key ${groupFingerprint(k.fingerprint)}${k.key_id === id ? "" : ", subkey " + groupFingerprint(id)}`
      : keys
        ? "Not in this instance's keyring"
        : "";

    return (
      <Tooltip key={i} title={title}>
        <Typography variant="caption" sx={{ display: "block" }}>
          {emails ? <>{emails} </> : null}
          <Box
            component="code"
            sx={{ whiteSpace: "nowrap", color: emails ? "text.secondary" : undefined }}
          >
            {groupFingerprint(id)}
          </Box>
        </Typography>
      </Tooltip>
    );
  });
}

// useGPGKeys loads the keyring once enabled, to name recipients by email.
// It stays null when the keyring can't be listed (the listing is admin
// only); recipients then show as key IDs alone.
function useGPGKeys(enabled: boolean): GPGKeyJSON[] | null {
  const [keys, setKeys] = useState<GPGKeyJSON[] | null>(null);

  useEffect(() => {
    if (!enabled) return;

    let cancelled = false;

    apiFetchJSON<GPGKeyListJSON>("/api/gpg-keys")
      .then((d) => {
        if (!cancelled) setKeys(d.keys);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, [enabled]);

  return keys;
}

// ReceiverFilesTable lists the files stored on this instance's receiver id
// (nothing while id is null), each with a download link behind a
// confirmation when canDownload. Changing reloadKey reloads the listing.
export function ReceiverFilesTable({
  id,
  canDownload,
  reloadKey = 0,
}: {
  id: string | null;
  canDownload: boolean;
  reloadKey?: number;
}) {
  // The listing is stored with the receiver id and reload it belongs to, so
  // switching to another receiver shows "loading…" (files === null) until
  // its own listing arrives, without resetting state inside the effect.
  const [loaded, setLoaded] = useState<{
    id: string;
    reloadKey: number;
    files: ReceiverFile[];
    error: boolean;
  } | null>(null);
  const current = loaded && loaded.id === id && loaded.reloadKey === reloadKey ? loaded : null;
  const files = current?.files ?? null;
  const keys = useGPGKeys(id !== null);
  const [pendingDownload, setPendingDownload] = useState<{ id: string; key: string } | null>(null);

  useEffect(() => {
    if (!id) return;

    let cancelled = false;

    apiFetchJSON<ReceiverFile[]>("/api/receivers/" + encodeURIComponent(id) + "/files")
      .then((f) => {
        if (!cancelled) setLoaded({ id, reloadKey, files: f || [], error: false });
      })
      .catch(() => {
        if (!cancelled) setLoaded({ id, reloadKey, files: [], error: true });
      });

    return () => {
      cancelled = true;
    };
  }, [id, reloadKey]);

  if (!id) return null;

  if (files === null) return <Typography color="text.secondary">loading…</Typography>;

  if (current?.error) return <Typography color="error">listing files failed</Typography>;

  if (!files.length) return <Typography color="text.secondary">no files stored</Typography>;

  return (
    <>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Key</TableCell>
            <TableCell>Size</TableCell>
            <TableCell>Modified</TableCell>
            <TableCell>Expires</TableCell>
            <TableCell>
              <Tooltip title="Key IDs the file is encrypted to, from its OpenPGP header. Usually an encryption subkey's ID, not the primary key's.">
                <span>Encrypted for</span>
              </Tooltip>
            </TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {groupFiles(files).map((g) => [
            g.isMultiFile ? (
              <TableRow key={"backup:" + g.key} sx={{ bgcolor: "action.hover" }}>
                <TableCell sx={{ overflowWrap: "anywhere", fontWeight: 500 }}>{g.key}</TableCell>
                <TableCell>{fmtSize(g.files.reduce((n, f) => n + f.size, 0))}</TableCell>
                <TableCell colSpan={4}>
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
                  <RecipientKeyIDs ids={f.recipient_key_ids} keys={keys} />
                </TableCell>
                <TableCell>
                  {canDownload ? (
                    <Link
                      component="button"
                      variant="body2"
                      underline="hover"
                      onClick={() => setPendingDownload({ id, key: objectPath(f) })}
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
