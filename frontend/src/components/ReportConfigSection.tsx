import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { ReportConfigJSON } from "../api/types";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Chip from "@mui/material/Chip";
import FormControlLabel from "@mui/material/FormControlLabel";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import Switch from "@mui/material/Switch";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

// PRESETS are common schedules offered as one-click shortcuts; any 5-field
// cron expression (evaluated in UTC) or descriptor is accepted.
const PRESETS = [
  { label: "Daily 07:00", schedule: "0 7 * * *" },
  { label: "Every 6 hours", schedule: "0 */6 * * *" },
  { label: "Mondays 07:00", schedule: "0 7 * * 1" },
  { label: "1st of month", schedule: "0 7 1 * *" },
];

interface ReportForm {
  enabled: boolean;
  schedule: string;
  notifications: string[];
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function ReportConfigSection() {
  const [data, setData] = useState<ReportConfigJSON | null>(null);
  const [form, setForm] = useState<ReportForm | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const refresh = useCallback(() => {
    apiFetchJSON<ReportConfigJSON>("/api/report-config")
      .then((d) => {
        setData(d);
        setForm({ enabled: d.enabled, schedule: d.schedule, notifications: d.notifications });
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
        "/api/report-config",
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ...form, schedule: form.schedule.trim() }),
        },
        "saving report settings failed",
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

  const set = (patch: Partial<ReportForm>) => {
    setSaved(false);
    setForm({ ...form, ...patch });
  };
  const options = data.notification_ids;

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack spacing={2}>
          {data.error ? (
            <Alert severity="warning">
              The saved settings are invalid, so the report is off: {data.error}
            </Alert>
          ) : null}
          {saveError ? <Alert severity="error">{saveError}</Alert> : null}
          {saved ? (
            <Alert severity="success">Saved. The new schedule applies immediately.</Alert>
          ) : null}

          <FormControlLabel
            control={
              <Switch checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            }
            label="Send the report"
          />

          <TextField
            label="Schedule"
            value={form.schedule}
            onChange={(e) => set({ schedule: e.target.value })}
            placeholder={data.default_schedule}
            helperText={`5-field cron expression (minute hour day month weekday), always in UTC; descriptors like "@daily" work too. Empty means ${data.default_schedule}.`}
            slotProps={{ htmlInput: { spellCheck: false, style: { fontFamily: "monospace" } } }}
          />
          <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap" }}>
            {PRESETS.map((p) => (
              <Chip
                key={p.schedule}
                label={p.label}
                size="small"
                variant={form.schedule.trim() === p.schedule ? "filled" : "outlined"}
                onClick={() => set({ schedule: p.schedule })}
              />
            ))}
          </Stack>

          <TextField
            select
            label="Send to"
            value={form.notifications}
            onChange={(e) => {
              const v = e.target.value as unknown as string | string[];
              set({ notifications: typeof v === "string" ? v.split(",") : v });
            }}
            helperText={
              options.length
                ? "An email gets the full report; a webhook gets a JSON summary (or its own body)."
                : "Create a notification first, on the Notifications page."
            }
            disabled={!options.length}
            slotProps={{ select: { multiple: true } }}
          >
            {options.map((id) => (
              <MenuItem key={id} value={id}>
                {id}
              </MenuItem>
            ))}
          </TextField>

          <Stack
            direction={{ xs: "column", sm: "row" }}
            spacing={1}
            sx={{ justifyContent: "space-between", alignItems: { sm: "center" } }}
          >
            <Typography variant="body2" color="text.secondary">
              {data.next_run ? <>Next report: {fmtTime(data.next_run)}. </> : <>Report is off. </>}
              {data.updated_at ? (
                <>
                  Last changed {fmtTime(data.updated_at)} by {data.updated_by}.
                </>
              ) : null}
            </Typography>
            <Button
              variant="contained"
              onClick={() => void save()}
              disabled={saving || (form.enabled && !form.notifications.length)}
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
