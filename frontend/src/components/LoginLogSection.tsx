import { useMemo, useState } from "react";
import type { LoginEventJSON } from "../api/types";
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

type SortKey = "at" | "username" | "method" | "result";

const comparators: Record<SortKey, (a: LoginEventJSON, b: LoginEventJSON) => number> = {
  at: (a, b) => new Date(a.at).getTime() - new Date(b.at).getTime(),
  username: (a, b) => a.username.localeCompare(b.username),
  method: (a, b) => a.method.localeCompare(b.method),
  result: (a, b) => Number(a.success) - Number(b.success),
};

export function LoginLogSection({ events }: { events: LoginEventJSON[] }) {
  const [username, setUsername] = useState("");
  const [method, setMethod] = useState("");
  const [result, setResult] = useState("");

  const usernameFilter = username.trim().toLowerCase();
  // Memoized so useSortedRows' sort only re-runs when the rows or a
  // filter actually changed, not on every render.
  const filtered = useMemo(
    () =>
      events.filter((ev) => {
        if (usernameFilter && ev.username.toLowerCase().indexOf(usernameFilter) === -1)
          return false;
        if (method && ev.method !== method) return false;
        if (result === "success" && !ev.success) return false;
        if (result === "failed" && ev.success) return false;
        return true;
      }),
    [events, usernameFilter, method, result],
  );

  const { sorted, sortKey, sortDir, toggleSort } = useSortedRows<LoginEventJSON, SortKey>(
    filtered,
    comparators,
    "at",
  );

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no login events recorded yet
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
          sx={{ flex: "1 1 220px" }}
        />
        <Select
          size="small"
          value={method}
          onChange={(e) => setMethod(e.target.value)}
          displayEmpty
          sx={{ minWidth: 160 }}
        >
          <MenuItem value="">All methods</MenuItem>
          <MenuItem value="password">Password</MenuItem>
          <MenuItem value="oidc">SSO</MenuItem>
          <MenuItem value="api-token">API token</MenuItem>
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
                label="Username"
                sortKey="username"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
              />
              <SortableHeaderCell<SortKey>
                label="Method"
                sortKey="method"
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
              <TableCell>Remote address</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {sorted.length === 0 ? (
              <TableRow>
                <TableCell colSpan={5} align="center" sx={{ color: "text.secondary" }}>
                  No matching events
                </TableCell>
              </TableRow>
            ) : (
              sorted.map((ev, i) => (
                <TableRow key={i}>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(ev.at)}</TableCell>
                  <TableCell>{ev.username || "(unknown)"}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.method}</TableCell>
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
