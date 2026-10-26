import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON, apiFetchOK } from "../api/client";
import type { ServerConfigListJSON, StatusPushConfigJSON } from "../api/types";
import { fmtTime } from "../lib/format";
import Alert from "@mui/material/Alert";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import FormControlLabel from "@mui/material/FormControlLabel";
import MenuItem from "@mui/material/MenuItem";
import Stack from "@mui/material/Stack";
import Switch from "@mui/material/Switch";
import TextField from "@mui/material/TextField";
import Typography from "@mui/material/Typography";

// PushForm keeps the interval as typed; empty means the default.
interface PushForm {
  enabled: boolean;
  server: string;
  interval: string;
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function intervalValid(value: string, data: StatusPushConfigJSON): boolean {
  const v = value.trim();
  if (v === "" || v === "0") return true;
  const n = Number(v);
  return /^\d+$/.test(v) && n >= data.min_interval_seconds && n <= data.max_interval_seconds;
}

export function StatusPushConfigSection() {
  const [data, setData] = useState<StatusPushConfigJSON | null>(null);
  const [form, setForm] = useState<PushForm | null>(null);
  // remoteServers are the remote servers the push may go to; null while
  // they couldn't be listed, the server is then typed in.
  const [remoteServers, setRemoteServers] = useState<string[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const refresh = useCallback(() => {
    apiFetchJSON<StatusPushConfigJSON>("/api/status-push-config")
      .then((d) => {
        setData(d);
        setForm({
          enabled: d.enabled,
          server: d.server,
          interval: d.interval_seconds ? String(d.interval_seconds) : "",
        });
        setLoadError(null);
      })
      .catch((err: unknown) => setLoadError(errorText(err)));
  }, []);

  useEffect(() => {
    refresh();
    apiFetchJSON<ServerConfigListJSON>("/api/server-configs")
      .then((l) =>
        setRemoteServers(
          l.servers.filter((s) => s.type === "remote" && !s.error).map((s) => s.name),
        ),
      )
      .catch(() => setRemoteServers(null));
  }, [refresh]);

  async function save() {
    if (!form) return;

    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      await apiFetchOK(
        "/api/status-push-config",
        {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            enabled: form.enabled,
            server: form.server.trim(),
            interval_seconds: Number(form.interval.trim() || 0),
          }),
        },
        "saving status push settings failed",
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

  const set = (patch: Partial<PushForm>) => {
    setSaved(false);
    setForm({ ...form, ...patch });
  };
  const validInterval = intervalValid(form.interval, data);
  const valid = validInterval && (!form.enabled || form.server.trim() !== "");
  // Keep a stored server selectable even if it's no longer a usable one,
  // so the form shows what's saved.
  const serverOptions =
    remoteServers && form.server && !remoteServers.includes(form.server)
      ? [form.server, ...remoteServers]
      : remoteServers;

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack spacing={3}>
          {saveError ? <Alert severity="error">{saveError}</Alert> : null}
          {saved ? (
            <Alert severity="success">Saved. The next push goes out right away.</Alert>
          ) : null}

          <Typography variant="body2" color="text.secondary">
            Pushes this instance&apos;s job and receiver status to a central instance, which shows
            it read-only on its dashboard. The central instance must list this one as a trusted
            server, using the server ID and public key from this instance&apos;s Identity page. It
            never connects back, so this instance can sit behind NAT or a firewall.
          </Typography>

          <FormControlLabel
            control={
              <Switch checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            }
            label="Push status to a central instance"
          />

          <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
            {serverOptions ? (
              <TextField
                select
                label="Central instance"
                value={form.server}
                onChange={(e) => set({ server: e.target.value })}
                error={form.enabled && !form.server}
                helperText={
                  serverOptions.length
                    ? "A remote server: its endpoint and TLS settings are used."
                    : "Add a remote server pointing at the central instance first (Settings → Servers)."
                }
                sx={{ flex: 1 }}
              >
                {serverOptions.map((name) => (
                  <MenuItem key={name} value={name}>
                    {name}
                  </MenuItem>
                ))}
              </TextField>
            ) : (
              <TextField
                label="Central instance (remote server name)"
                value={form.server}
                onChange={(e) => set({ server: e.target.value })}
                error={form.enabled && !form.server.trim()}
                helperText="The name of a remote server: its endpoint and TLS settings are used."
                sx={{ flex: 1 }}
              />
            )}
            <TextField
              label="Push at least every (seconds)"
              value={form.interval}
              onChange={(e) => set({ interval: e.target.value })}
              placeholder={String(data.default_interval_seconds)}
              error={!validInterval}
              helperText={
                validInterval
                  ? `Changes are pushed within seconds; this is the heartbeat. Empty means ${data.default_interval_seconds}.`
                  : `Enter a whole number between ${data.min_interval_seconds} and ${data.max_interval_seconds}.`
              }
              slotProps={{ htmlInput: { inputMode: "numeric" } }}
              sx={{ flex: 1 }}
            />
          </Stack>

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
                <>Never set up: this instance doesn&apos;t push its status.</>
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
