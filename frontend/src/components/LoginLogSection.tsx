import type { LoginEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { fmtTime } from "../lib/format";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Typography from "@mui/material/Typography";

export function LoginLogSection({ events }: { events: LoginEventJSON[] }) {
  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no login events recorded yet
      </Typography>
    );
  }

  return (
    <TableContainer component={Paper} variant="outlined">
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Time</TableCell>
            <TableCell>Username</TableCell>
            <TableCell>Method</TableCell>
            <TableCell>Result</TableCell>
            <TableCell>Remote address</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {events.map((ev, i) => (
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
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
