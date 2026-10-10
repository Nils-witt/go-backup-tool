import { useMemo, useState } from "react";
import type { AuditEventJSON } from "../api/types";
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

type SortKey = "at" | "username" | "action" | "resource" | "result";

const comparators: Record<SortKey, (a: AuditEventJSON, b: AuditEventJSON) => number> = {
  at: (a, b) => new Date(a.at).getTime() - new Date(b.at).getTime(),
  username: (a, b) => a.username.localeCompare(b.username),
  action: (a, b) => a.action.localeCompare(b.action),
  resource: (a, b) => a.resource.localeCompare(b.resource) || a.target.localeCompare(b.target),
  result: (a, b) => Number(a.success) - Number(b.success),
};

export function AuditLogSection({ events }: { events: AuditEventJSON[] }) {
  const [search, setSearch] = useState("");
  const [resource, setResource] = useState("");
  const [result, setResult] = useState("");

  // The resource filter offers every resource seen in the loaded events,
  // rather than a hard-coded list that could drift from the server's routes.
  const resources = useMemo(() => [...new Set(events.map((ev) => ev.resource))].sort(), [events]);

  const searchFilter = search.trim().toLowerCase();
  // Memoized so useSortedRows' sort only re-runs when the rows or a
  // filter actually changed, not on every render.
  const filtered = useMemo(
    () =>
      events.filter((ev) => {
        if (
          searchFilter &&
          ev.username.toLowerCase().indexOf(searchFilter) === -1 &&
          ev.target.toLowerCase().indexOf(searchFilter) === -1
        )
          return false;
        if (resource && ev.resource !== resource) return false;
        if (result === "success" && !ev.success) return false;
        if (result === "failed" && ev.success) return false;
        return true;
      }),
    [events, searchFilter, resource, result],
  );

  const { sorted, sortKey, sortDir, toggleSort } = useSortedRows<AuditEventJSON, SortKey>(
    filtered,
    comparators,
    "at",
  );

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no changes recorded yet
      </Typography>
    );
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap" }}>
        <TextField
          size="small"
          placeholder="Filter by user or item…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          sx={{ flex: "1 1 220px" }}
        />
        <Select
          size="small"
          value={resource}
          onChange={(e) => setResource(e.target.value)}
          displayEmpty
          sx={{ minWidth: 180 }}
        >
          <MenuItem value="">All resources</MenuItem>
          {resources.map((r) => (
            <MenuItem key={r} value={r}>
              {r}
            </MenuItem>
          ))}
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
                label="User"
                sortKey="username"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
              />
              <SortableHeaderCell<SortKey>
                label="Action"
                sortKey="action"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="Resource"
                sortKey="resource"
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
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.action}</TableCell>
                  <TableCell sx={{ overflowWrap: "anywhere" }}>
                    {ev.resource}
                    {ev.target ? (
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ display: "block" }}
                      >
                        {ev.target}
                      </Typography>
                    ) : null}
                  </TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    {ev.success ? (
                      <StatusChip state="ok" label="success" />
                    ) : (
                      <StatusChip state="failed" label={`failed (${ev.status})`} />
                    )}
                    {ev.detail ? (
                      <Typography
                        variant="caption"
                        color="error"
                        sx={{ display: "block", overflowWrap: "anywhere", whiteSpace: "normal" }}
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
