import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { ReportJSON, ReportSummaryJSON } from "../api/types";
import { StatusChip } from "./StatusChip";
import { fmtSize, fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Dialog from "@mui/material/Dialog";
import DialogActions from "@mui/material/DialogActions";
import DialogContent from "@mui/material/DialogContent";
import DialogTitle from "@mui/material/DialogTitle";
import MenuItem from "@mui/material/MenuItem";
import Paper from "@mui/material/Paper";
import Stack from "@mui/material/Stack";
import Table from "@mui/material/Table";
import TableBody from "@mui/material/TableBody";
import TableCell from "@mui/material/TableCell";
import TableContainer from "@mui/material/TableContainer";
import TableHead from "@mui/material/TableHead";
import TableRow from "@mui/material/TableRow";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

// PERIODS are the windows a report can be generated for, in hours.
const PERIODS = [
  { label: "Last 24 hours", hours: 24 },
  { label: "Last 7 days", hours: 24 * 7 },
  { label: "Last 30 days", hours: 24 * 30 },
];

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function fmtPeriod(r: ReportSummaryJSON): string {
  return `${fmtTime(r.period_start)} – ${fmtTime(r.period_end)}`;
}

// fmtStaleAfter formats a receiver's stale-after as whole days or hours.
function fmtStaleAfter(seconds: number): string {
  const hours = seconds / 3600;
  if (hours >= 24 && hours % 24 === 0) return `${hours / 24}d`;
  return Number.isInteger(hours) ? `${hours}h` : `${Math.round(seconds / 60)}m`;
}

function triggerLabel(r: ReportSummaryJSON): string {
  if (r.trigger === "manual")
    return r.triggered_by ? `on demand by ${r.triggered_by}` : "on demand";
  return "scheduled";
}

function ErrorsChip({ count }: { count: number }) {
  return count > 0 ? (
    <StatusChip state="failed" label={`${count} error${count === 1 ? "" : "s"}`} />
  ) : (
    <StatusChip state="ok" label="no errors" />
  );
}

export function ReportHistorySection() {
  const [reports, setReports] = useState<ReportSummaryJSON[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [hours, setHours] = useState(PERIODS[0].hours);
  const [generating, setGenerating] = useState(false);
  const [generateError, setGenerateError] = useState<string | null>(null);
  const [openId, setOpenId] = useState<number | null>(null);
  // generated is the report just generated, shown without refetching it.
  const [generated, setGenerated] = useState<ReportJSON | null>(null);

  const refresh = useCallback(() => {
    apiFetchJSON<ReportSummaryJSON[]>("/api/reports")
      .then((d) => {
        setReports(d);
        setLoadError(null);
      })
      .catch((err: unknown) => setLoadError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  async function generate() {
    setGenerating(true);
    setGenerateError(null);
    try {
      const res = await apiFetchOK(
        "/api/reports",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ hours }),
        },
        "generating the report failed",
      );
      const report = (await res.json()) as ReportJSON;
      setGenerated(report);
      setOpenId(report.id);
      refresh();
    } catch (err) {
      setGenerateError(errorText(err));
    } finally {
      setGenerating(false);
    }
  }

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack spacing={2}>
          <Stack
            direction={{ xs: "column", sm: "row" }}
            spacing={1.5}
            sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
          >
            <Stack spacing={0.5}>
              <Typography variant="h6">Past reports</Typography>
              <Typography variant="body2" color="text.secondary">
                Every scheduled report, and any generated here. Generating one doesn't send it.
              </Typography>
            </Stack>
            <Stack direction="row" spacing={1} sx={{ flexShrink: 0 }}>
              <TextField
                select
                size="small"
                label="Period"
                value={hours}
                onChange={(e) => setHours(Number(e.target.value))}
                sx={{ minWidth: 160 }}
              >
                {PERIODS.map((p) => (
                  <MenuItem key={p.hours} value={p.hours}>
                    {p.label}
                  </MenuItem>
                ))}
              </TextField>
              <Button variant="contained" onClick={() => void generate()} disabled={generating}>
                Generate now
              </Button>
            </Stack>
          </Stack>

          {generateError ? <Alert severity="error">{generateError}</Alert> : null}
          {loadError ? <Alert severity="error">{loadError}</Alert> : null}

          {reports && !reports.length ? (
            <Typography color="text.secondary" sx={{ py: 3, textAlign: "center" }}>
              No reports kept yet
            </Typography>
          ) : null}

          {reports && reports.length ? (
            <TableContainer component={Paper} variant="outlined">
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>Generated</TableCell>
                    <TableCell>Period</TableCell>
                    <TableCell>Made</TableCell>
                    <TableCell align="right">Receivers</TableCell>
                    <TableCell align="right">Jobs</TableCell>
                    <TableCell align="right">Stale</TableCell>
                    <TableCell>Errors</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {reports.map((r) => (
                    <TableRow
                      key={r.id}
                      hover
                      onClick={() => setOpenId(r.id)}
                      sx={{ cursor: "pointer" }}
                    >
                      <TableCell sx={{ whiteSpace: "nowrap" }}>{fmtTime(r.generated_at)}</TableCell>
                      <TableCell sx={{ color: "text.secondary" }}>{fmtPeriod(r)}</TableCell>
                      <TableCell sx={{ whiteSpace: "nowrap" }}>{triggerLabel(r)}</TableCell>
                      <TableCell align="right">{r.receivers}</TableCell>
                      <TableCell align="right">{r.jobs}</TableCell>
                      <TableCell align="right">{r.stale}</TableCell>
                      <TableCell sx={{ whiteSpace: "nowrap" }}>
                        <ErrorsChip count={r.receiver_errors + r.job_errors} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          ) : null}
        </Stack>
      </CardContent>

      <ReportDialog
        id={openId}
        initial={generated && generated.id === openId ? generated : null}
        onClose={() => setOpenId(null)}
      />
    </Card>
  );
}

// ReportDialog shows one kept report's sections, fetching it unless
// initial already holds it.
function ReportDialog({
  id,
  initial,
  onClose,
}: {
  id: number | null;
  initial: ReportJSON | null;
  onClose: () => void;
}) {
  // loaded is the last fetch's outcome, for the id it was made for.
  const [loaded, setLoaded] = useState<{ id: number; report?: ReportJSON; error?: string } | null>(
    null,
  );

  useEffect(() => {
    if (id === null || initial) return;

    let cancelled = false;
    apiFetchJSON<ReportJSON>(`/api/reports/${id}`)
      .then((report) => {
        if (!cancelled) setLoaded({ id, report });
      })
      .catch((err: unknown) => {
        if (!cancelled) setLoaded({ id, error: errorText(err) });
      });

    return () => {
      cancelled = true;
    };
  }, [id, initial]);

  const current = loaded && loaded.id === id ? loaded : null;
  const report = initial ?? current?.report ?? null;
  const error = initial ? null : (current?.error ?? null);

  return (
    <Dialog open={id !== null} onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>Report</DialogTitle>
      <DialogContent dividers>
        {error ? <Alert severity="error">{error}</Alert> : null}
        {report ? <ReportBody report={report} /> : null}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>Close</Button>
      </DialogActions>
    </Dialog>
  );
}

function ReportBody({ report }: { report: ReportJSON }) {
  const c = report.content;

  return (
    <Stack spacing={3}>
      <Typography variant="body2" color="text.secondary">
        {fmtPeriod(report)} · generated {fmtTime(report.generated_at)}, {triggerLabel(report)}
        {report.notifications.length ? <> · sent to {report.notifications.join(", ")}</> : null}
      </Typography>

      <ReportTable
        title="Receivers"
        empty="No receivers configured."
        head={["Receiver", "Files", "Size", "Errors"]}
        rows={c.receivers.map((r) => [r.id, r.files_received, fmtSize(r.bytes_received), r.errors])}
      />
      <ReportTable
        title="Stale receivers"
        empty="No receiver was stale."
        head={["Receiver", "Last received", "Stale after"]}
        rows={c.stale.map((s) => [
          s.id,
          fmtTime(s.last_seen),
          fmtStaleAfter(s.stale_after_seconds),
        ])}
      />
      <ReportTable
        title="Receiver errors"
        empty="No receiver errors recorded."
        head={["Time", "Receiver", "Request", "Key", "Error"]}
        rows={c.receiver_errors.map((e) => [fmtTime(e.at), e.receiver_id, e.kind, e.key, e.error])}
      />
      <ReportTable
        title="Jobs"
        empty="No jobs configured."
        head={["Job", "Runs", "Size", "Errors"]}
        rows={c.jobs.map((j) => [j.id, j.runs_completed, fmtSize(j.bytes_written), j.errors])}
      />
      <ReportTable
        title="Job errors"
        empty="No job errors recorded."
        head={["Time", "Job", "Error"]}
        rows={c.job_errors.map((e) => [fmtTime(e.at), e.job_name, e.error])}
      />
    </Stack>
  );
}

function ReportTable({
  title,
  empty,
  head,
  rows,
}: {
  title: string;
  empty: string;
  head: string[];
  rows: ReactNode[][];
}) {
  return (
    <Stack spacing={1}>
      <Typography variant="subtitle2">{title}</Typography>
      {rows.length ? (
        <TableContainer component={Paper} variant="outlined">
          <Table size="small">
            <TableHead>
              <TableRow>
                {head.map((h) => (
                  <TableCell key={h}>{h}</TableCell>
                ))}
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((row, i) => (
                <TableRow key={i}>
                  {row.map((cell, j) => (
                    <TableCell key={j} sx={{ wordBreak: "break-word" }}>
                      {cell}
                    </TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableContainer>
      ) : (
        <Typography variant="body2" color="text.secondary">
          {empty}
        </Typography>
      )}
    </Stack>
  );
}
