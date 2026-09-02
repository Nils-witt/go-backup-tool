import { useState } from "react";
import type { TargetRunEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
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

export function TargetRunLogSection({ events }: { events: TargetRunEventJSON[] }) {
  const [job, setJob] = useState("");
  const [target, setTarget] = useState("");
  const [result, setResult] = useState("");

  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no target runs recorded yet
      </Typography>
    );
  }

  const jobFilter = job.trim().toLowerCase();
  const targetFilter = target.trim().toLowerCase();
  const filtered = events.filter((ev) => {
    if (jobFilter && ev.job_name.toLowerCase().indexOf(jobFilter) === -1) return false;
    if (targetFilter && ev.target.toLowerCase().indexOf(targetFilter) === -1) return false;
    if (result === "success" && !ev.success) return false;
    if (result === "failed" && ev.success) return false;
    return true;
  });

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={1.5} sx={{ flexWrap: "wrap" }}>
        <TextField
          size="small"
          placeholder="Filter by job…"
          value={job}
          onChange={(e) => setJob(e.target.value)}
          sx={{ flex: "1 1 200px" }}
        />
        <TextField
          size="small"
          placeholder="Filter by target…"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
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
              <TableCell>Time</TableCell>
              <TableCell>Job</TableCell>
              <TableCell>Target</TableCell>
              <TableCell>Result</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {filtered.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} align="center" sx={{ color: "text.secondary" }}>
                  No matching runs
                </TableCell>
              </TableRow>
            ) : (
              filtered.map((ev, i) => (
                <TableRow key={i}>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(ev.at)}</TableCell>
                  <TableCell>{ev.job_name}</TableCell>
                  <TableCell>{ev.target}</TableCell>
                  <TableCell sx={{ whiteSpace: "nowrap" }}>
                    <StatusChip state={ev.state} />
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
