import type { ReceiverEventJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { fmtTime, fmtSize } from "../lib/format";
import Paper from "@mui/material/Paper";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import Typography from "@mui/material/Typography";

export function ReceiverLogSection({ events }: { events: ReceiverEventJSON[] }) {
  if (!events.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no receiver events recorded yet
      </Typography>
    );
  }

  return (
    <TableContainer component={Paper} variant="outlined">
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Time</TableCell>
            <TableCell>Receiver</TableCell>
            <TableCell>Kind</TableCell>
            <TableCell>File</TableCell>
            <TableCell>Size</TableCell>
            <TableCell>Result</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {events.map((ev, i) => (
            <TableRow key={i}>
              <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(ev.at)}</TableCell>
              <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.receiver_id}</TableCell>
              <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.kind}</TableCell>
              <TableCell sx={{ overflowWrap: "anywhere" }}>{ev.key}</TableCell>
              <TableCell sx={{ whiteSpace: "nowrap" }}>{ev.size ? fmtSize(ev.size) : ""}</TableCell>
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
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
