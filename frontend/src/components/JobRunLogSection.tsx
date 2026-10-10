import { useMemo, useState } from "react";
import type { JobRunEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { SortableHeaderCell } from "./SortableHeaderCell";
import { useSortedRows } from "../hooks/useSortedRows";
import { fmtDuration, fmtSize, fmtTime } from "../lib/format";
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

type SortKey = "end" | "job_name" | "duration" | "size" | "result";

const comparators: Record<SortKey, (a: JobRunEventJSON, b: JobRunEventJSON) => number> = {
  end: (a, b) => new Date(a.end).getTime() - new Date(b.end).getTime(),
  job_name: (a, b) => a.job_name.localeCompare(b.job_name),
  duration: (a, b) =>
    new Date(a.end).getTime() -
    new Date(a.start).getTime() -
    (new Date(b.end).getTime() - new Date(b.start).getTime()),
  size: (a, b) => a.size - b.size,
  result: (a, b) => Number(a.success) - Number(b.success),
};

export function JobRunLogSection({ events }: { events: JobRunEventJSON[] }) {
  const [job, setJob] = useState("");
  const [result, setResult] = useState("");

  const jobFilter = job.trim().toLowerCase();
  // Memoized so useSortedRows' sort only re-runs when the rows or a
  // filter actually changed, not on every render.
  const filtered = useMemo(
    () =>
      events.filter((ev) => {
        if (jobFilter && ev.job_name.toLowerCase().indexOf(jobFilter) === -1) return false;
        if (result === "success" && !ev.success) return false;
        if (result === "failed" && ev.success) return false;
        return true;
      }),
    [events, jobFilter, result],
  );

  const { sorted, sortKey, sortDir, toggleSort } = useSortedRows<JobRunEventJSON, SortKey>(
    filtered,
    comparators,
    "end",
  );

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no job runs recorded yet
      </Typography>
    );
  }

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap" }}>
        <TextField
          size="small"
          placeholder="Filter by job…"
          value={job}
          onChange={(e) => setJob(e.target.value)}
          sx={{ flex: "1 1 220px" }}
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
                sortKey="end"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
              />
              <SortableHeaderCell<SortKey>
                label="Job"
                sortKey="job_name"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
              />
              <SortableHeaderCell<SortKey>
                label="Duration"
                sortKey="duration"
                activeKey={sortKey}
                direction={sortDir}
                onSort={toggleSort}
                whiteSpace
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
                <TableCell colSpan={5} align="center" sx={{ color: "text.secondary" }}>
                  No matching runs
                </TableCell>
              </TableRow>
            ) : (
              sorted.map((ev, i) => (
                <TableRow key={i}>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(ev.end)}</TableCell>
                  <TableCell>{ev.job_name}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    {fmtDuration(new Date(ev.end).getTime() - new Date(ev.start).getTime())}
                  </TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtSize(ev.size)}</TableCell>
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
