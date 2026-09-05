import { useState } from "react";
import type { DownloadEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { SortableHeaderCell } from "./SortableHeaderCell";
import { useSortedRows } from "../hooks/useSortedRows";
import { fmtTime } from "../lib/format";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Select from "@mui/material/Select";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

type SortKey = "at" | "username" | "receiver_id" | "key" | "result";

const comparators: Record<SortKey, (a: DownloadEventJSON, b: DownloadEventJSON) => number> = {
  at: (a, b) => new Date(a.at).getTime() - new Date(b.at).getTime(),
  username: (a, b) => a.username.localeCompare(b.username),
  receiver_id: (a, b) => a.receiver_id.localeCompare(b.receiver_id),
  key: (a, b) => a.key.localeCompare(b.key),
  result: (a, b) => Number(a.success) - Number(b.success),
};

export function DownloadLogSection({ events }: { events: DownloadEventJSON[] }) {
  const [username, setUsername] = useState("");
  const [receiver, setReceiver] = useState("");
  const [key, setKey] = useState("");
  const [result, setResult] = useState("");

  const usernameFilter = username.trim().toLowerCase();
  const receiverFilter = receiver.trim().toLowerCase();
  const keyFilter = key.trim().toLowerCase();
  const filtered = events.filter((ev) => {
    if (usernameFilter && ev.username.toLowerCase().indexOf(usernameFilter) === -1) return false;
    if (receiverFilter && ev.receiver_id.toLowerCase().indexOf(receiverFilter) === -1) return false;
    if (keyFilter && ev.key.toLowerCase().indexOf(keyFilter) === -1) return false;
    if (result === "success" && !ev.success) return false;
    if (result === "failed" && ev.success) return false;
    return true;
  });

  const { sorted, sortKey, sortDir, toggleSort } = useSortedRows<DownloadEventJSON, SortKey>(
    filtered,
    comparators,
    "at",
  );

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no download events recorded yet
      </Typography>
    );
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap" }}>
        <TextField
          size="small"
          placeholder="Filter by username…"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          sx={{ flex: "1 1 200px" }}
        />
        <TextField
          size="small"
          placeholder="Filter by receiver…"
          value={receiver}
          onChange={(e) => setReceiver(e.target.value)}
          sx={{ flex: "1 1 200px" }}
        />
        <TextField
          size="small"
          placeholder="Filter by file…"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          sx={{ flex: "1 1 200px" }}
        />
        <Select
          size="small"
          value={result}
          onChange={(e) => setResult(e.target.value)}
          displayEmpty
          sx={{ minWidth: 160 }}
        >
          <MenuItem value="">All results</MenuItem>
          <MenuItem value="success">Success</MenuItem>
          <MenuItem value="failed">Failed</MenuItem>
        </Select>
      </Stack>
      <TableContainer component={Paper} variant="outlined">
        <Table size="small">
          <TableHead>
            <TableRow>
              <SortableHeaderCell<SortKey>
                label="Time"
                sortKey="at"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="Username"
                sortKey="username"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
              />
              <SortableHeaderCell<SortKey>
                label="Receiver"
                sortKey="receiver_id"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="File"
                sortKey="key"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
              />
              <SortableHeaderCell<SortKey>
                label="Result"
                sortKey="result"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <TableCell>Remote address</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {sorted.length === 0 ? (
              <TableRow>
                <TableCell colSpan={6} align="center" sx={{ color: "text.secondary" }}>
                  No matching events
                </TableCell>
              </TableRow>
            ) : (
              sorted.map((ev, i) => (
                <TableRow key={i}>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(ev.at)}</TableCell>
                  <TableCell>{ev.username || "(unknown)"}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.receiver_id}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>{ev.key}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    {ev.success ? (
                      <StatusChip state="ok" label="success" />
                    ) : (
                      <StatusChip state="failed" label="failed" />
                    )}
                    {ev.detail ? (
                      <Typography
                        variant="caption"
                        color="error"
                        sx={{ display: "block", overflowWrap: "anywhere" }}
                      >
                        {ev.detail}
                      </Typography>
                    ) : null}
                  </TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>{ev.remote_addr}</TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
    </Stack>
  );
}
