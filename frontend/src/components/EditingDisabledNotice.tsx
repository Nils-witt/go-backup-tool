import Alert from "@mui/material/Alert";

// EditingDisabledNotice explains why the jobs/servers/commands settings
// pages are read-only: without webui.job-editing they're managed in the
// config file, since each can run commands or write files on this machine.
export function EditingDisabledNotice() {
  return (
    <Alert severity="info">
      Read-only: managed in the config file (<code>jobs:</code>, <code>servers:</code>,{" "}
      <code>commands:</code>) and applied on restart. To manage them here instead, set{" "}
      <code>webui.job-editing: true</code>; the config file&apos;s entries are then imported once.
    </Alert>
  );
}
