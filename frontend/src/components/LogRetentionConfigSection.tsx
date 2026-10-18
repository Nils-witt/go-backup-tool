import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { LogRetention, LogRetentionConfigJSON } from "../api/types";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Stack from "@mui/material/Stack";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

// RetentionForm keeps each limit as typed; empty means 0, no limit.
type RetentionForm = Record<keyof LogRetention, string>;

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function toForm(r: LogRetention): RetentionForm {
  const field = (n: number) => (n ? String(n) : "");
  return {
    job_run_days: field(r.job_run_days),
    job_run_max_rows: field(r.job_run_max_rows),
    target_run_days: field(r.target_run_days),
    target_run_max_rows: field(r.target_run_max_rows),
  };
}

function fromForm(f: RetentionForm): LogRetention {
  const num = (s: string) => Number(s.trim() || 0);
  return {
    job_run_days: num(f.job_run_days),
    job_run_max_rows: num(f.job_run_max_rows),
    target_run_days: num(f.target_run_days),
    target_run_max_rows: num(f.target_run_max_rows),
  };
}

function isValid(value: string, limit: number): boolean {
  const v = value.trim();
  return v === "" || (/^\d+$/.test(v) && Number(v) <= limit);
}

// describe says what a log's limits keep, e.g. "30 days, at most 100 rows per job".
function describe(days: number, rows: number, per: string): string {
  if (!days && !rows) return "everything, forever";
  const parts = [];
  if (days) parts.push(`${days} day${days === 1 ? "" : "s"}`);
  if (rows) parts.push(`at most ${rows} rows per ${per}`);
  return parts.join(", ");
}

interface LimitFieldsProps {
  title: string;
  description: string;
  daysKey: keyof LogRetention;
  rowsKey: keyof LogRetention;
  per: string;
  form: RetentionForm;
  data: LogRetentionConfigJSON;
  set: (patch: Partial<RetentionForm>) => void;
}

function LimitFields({
  title,
  description,
  daysKey,
  rowsKey,
  per,
  form,
  data,
  set,
}: LimitFieldsProps) {
  const daysValid = isValid(form[daysKey], data.limit_days);
  const rowsValid = isValid(form[rowsKey], data.limit_rows);
  const current = fromForm(form);

  return (
    <Stack spacing={1.5}>
      <div>
        <Typography variant="subtitle1">{title}</Typography>
        <Typography variant="body2" color="text.secondary">
          {description} Keeps {describe(current[daysKey], current[rowsKey], per)}.
        </Typography>
      </div>
      <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
        <TextField
          label="Keep for (days)"
          value={form[daysKey]}
          onChange={(e) => set({ [daysKey]: e.target.value })}
          placeholder="No limit"
          error={!daysValid}
          helperText={
            daysValid
              ? `Empty or 0 means no age limit. Default: ${data.defaults[daysKey] || "no limit"}.`
              : `Enter a whole number of days, up to ${data.limit_days}.`
          }
          slotProps={{ htmlInput: { inputMode: "numeric" } }}
          sx={{ flex: 1 }}
        />
        <TextField
          label={`Max rows per ${per}`}
          value={form[rowsKey]}
          onChange={(e) => set({ [rowsKey]: e.target.value })}
          placeholder="No limit"
          error={!rowsValid}
          helperText={
            rowsValid
              ? `Empty or 0 means no row limit. Default: ${data.defaults[rowsKey] || "no limit"}.`
              : `Enter a whole number, up to ${data.limit_rows}.`
          }
          slotProps={{ htmlInput: { inputMode: "numeric" } }}
          sx={{ flex: 1 }}
        />
      </Stack>
    </Stack>
  );
}

export function LogRetentionConfigSection() {
  const [data, setData] = useState<LogRetentionConfigJSON | null>(null);
  const [form, setForm] = useState<RetentionForm | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const refresh = useCallback(() => {
    apiFetchJSON<LogRetentionConfigJSON>("/api/log-retention-config")
      .then((d) => {
        setData(d);
        setForm(toForm(d));
        setLoadError(null);
      })
      .catch((err: unknown) => setLoadError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  async function save() {
    if (!form) return;

    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      await apiFetchOK(
        "/api/log-retention-config",
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(fromForm(form)),
        },
        "saving log retention settings failed",
      );
      setSaved(true);
      refresh();
    } catch (err) {
      setSaveError(errorText(err));
    } finally {
      setSaving(false);
    }
  }

  if (loadError) return <Alert severity="error">{loadError}</Alert>;
  if (!data || !form) return null;

  const set = (patch: Partial<RetentionForm>) => {
    setSaved(false);
    setForm({ ...form, ...patch });
  };
  const valid =
    isValid(form.job_run_days, data.limit_days) &&
    isValid(form.job_run_max_rows, data.limit_rows) &&
    isValid(form.target_run_days, data.limit_days) &&
    isValid(form.target_run_max_rows, data.limit_rows);

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack spacing={3}>
          {saveError ? <Alert severity="error">{saveError}</Alert> : null}
          {saved ? (
            <Alert severity="success">
              Saved. Older rows are deleted right away, and then hourly.
            </Alert>
          ) : null}

          <LimitFields
            title="Job runs"
            description="One row per completed job run, shown on the Job runs page."
            daysKey="job_run_days"
            rowsKey="job_run_max_rows"
            per="job"
            form={form}
            data={data}
            set={set}
          />
          <LimitFields
            title="Target runs"
            description="One row per target of every run, shown on the Target runs page."
            daysKey="target_run_days"
            rowsKey="target_run_max_rows"
            per="target"
            form={form}
            data={data}
            set={set}
          />

          <Typography variant="body2" color="text.secondary">
            Whatever the limits, each job&apos;s latest run and latest successful run, and each
            target&apos;s latest run, are kept: the dashboard and missed-run catch-up rely on them.
          </Typography>

          <Stack
            direction={{ xs: "column", sm: "row" }}
            spacing={1}
            sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
          >
            <Typography variant="body2" color="text.secondary">
              {data.updated_at ? (
                <>
                  Last changed {fmtTime(data.updated_at)} by {data.updated_by}.
                </>
              ) : (
                <>Never changed: the defaults apply.</>
              )}
            </Typography>
            <Button
              variant="contained"
              onClick={() => void save()}
              disabled={saving || !valid}
              sx={{ flexShrink: 0 }}
            >
              Save
            </Button>
          </Stack>
        </Stack>
      </CardContent>
    </Card>
  );
}
