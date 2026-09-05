import { useState } from "react";
import type { ReceiverEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { SortableHeaderCell } from "./SortableHeaderCell";
import { useSortedRows } from "../hooks/useSortedRows";
import { fmtTime, fmtSize } from "../lib/format";
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

type SortKey = "at" | "receiver_id" | "kind" | "key" | "size" | "result";

const comparators: Record<SortKey, (a: ReceiverEventJSON, b: ReceiverEventJSON) => number> = {
  at: (a, b) => new Date(a.at).getTime() - new Date(b.at).getTime(),
  receiver_id: (a, b) => a.receiver_id.localeCompare(b.receiver_id),
  kind: (a, b) => a.kind.localeCompare(b.kind),
  key: (a, b) => a.key.localeCompare(b.key),
  size: (a, b) => a.size - b.size,
  result: (a, b) => Number(a.success) - Number(b.success),
};

export function ReceiverLogSection({ events }: { events: ReceiverEventJSON[] }) {
  const [receiver, setReceiver] = useState("");
  const [key, setKey] = useState("");
  const [kind, setKind] = useState("");
  const [result, setResult] = useState("");

  const receiverFilter = receiver.trim().toLowerCase();
  const keyFilter = key.trim().toLowerCase();
  const filtered = events.filter((ev) => {
    if (receiverFilter && ev.receiver_id.toLowerCase().indexOf(receiverFilter) === -1) return false;
    if (keyFilter && ev.key.toLowerCase().indexOf(keyFilter) === -1) return false;
    if (kind && ev.kind !== kind) return false;
    if (result === "success" && !ev.success) return false;
    if (result === "failed" && ev.success) return false;
    return true;
  });

  const { sorted, sortKey, sortDir, toggleSort } = useSortedRows<ReceiverEventJSON, SortKey>(
    filtered,
    comparators,
    "at",
  );

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no receiver events recorded yet
      </Typography>
    );
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap" }}>
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
          value={kind}
          onChange={(e) => setKind(e.target.value)}
          displayEmpty
          sx={{ minWidth: 160 }}
        >
          <MenuItem value="">All kinds</MenuItem>
          <MenuItem value="receive">Receive</MenuItem>
          <MenuItem value="delete">Delete</MenuItem>
        </Select>
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
                label="Receiver"
                sortKey="receiver_id"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="Kind"
                sortKey="kind"
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
                label="Size"
                sortKey="size"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="Result"
                sortKey="result"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
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
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.receiver_id}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.kind}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>{ev.key}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    {ev.size ? fmtSize(ev.size) : ""}
                  </TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    {ev.success ? (
                      <StatusChip state="ok" label="success" />
                    ) : (
                      <StatusChip state="failed" label="failed" />
                    )}
                    {ev.error ? (
                      <Typography
                        variant="caption"
                        color="error"
                        sx={{ display: "block", overflowWrap: "anywhere" }}
                      >
                        {ev.error}
                      </Typography>
                    ) : null}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
    </Stack>
  );
}
