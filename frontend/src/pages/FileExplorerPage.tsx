import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { ReceiverSnapshot } from "../api/types";
import { useAuth } from "../auth/useAuth";
import { usePoll } from "../hooks/usePoll";
import { PageHeader } from "../components/PageHeader";
import { ReceiverFilesTable } from "../components/ReceiverFilesTable";
import { StatusChip } from "../components/StatusChip";
import Box from "@mui/material/Box";
import IconButton from "@mui/material/IconButton";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import ListItemText from "@mui/material/ListItemText";
import Paper from "@mui/material/Paper";
import RefreshIcon from "@mui/icons-material/Refresh";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";

// FileExplorerPage browses the files stored on this instance's receivers:
// the receivers in a sidebar, the selected one's files in the center. The
// selection lives in the URL (?receiver=ID), so it survives a reload and
// can be linked to. Peers' receivers aren't listed: their files are on the
// other instance.
export function FileExplorerPage() {
  const session = useAuth();
  const receivers = usePoll<ReceiverSnapshot>("/api/receivers", 5000);
  const [params, setParams] = useSearchParams();
  const [reloadKey, setReloadKey] = useState(0);

  const sorted = [...receivers.data].sort((a, b) => a.id.localeCompare(b.id));
  const selected = params.get("receiver") ?? sorted[0]?.id ?? null;
  const selectedReceiver = sorted.find((r) => r.id === selected);

  return (
    <>
      <PageHeader title="File explorer" subtitle="Files stored on this instance's receivers." />
      <Stack direction={{ xs: "column", md: "row" }} spacing={2} sx={{ alignItems: "flex-start" }}>
        <Paper
          variant="outlined"
          sx={{ width: { xs: "100%", md: 260 }, flexShrink: 0, alignSelf: "stretch" }}
        >
          {!receivers.loaded ? (
            <Typography color="text.secondary" sx={{ p: 2 }}>
              loading…
            </Typography>
          ) : !sorted.length ? (
            <Typography color="text.secondary" sx={{ p: 2 }}>
              no receivers configured
            </Typography>
          ) : (
            <List dense disablePadding aria-label="Receivers">
              {sorted.map((r) => (
                <ListItemButton
                  key={r.id}
                  selected={r.id === selected}
                  onClick={() => setParams({ receiver: r.id }, { replace: true })}
                >
                  <ListItemText
                    primary={r.id}
                    secondary={r.path}
                    slotProps={{
                      primary: { sx: { overflowWrap: "anywhere" } },
                      secondary: { sx: { overflowWrap: "anywhere" } },
                    }}
                  />
                  <StatusChip state={r.stale ? "incomplete" : r.state} error={r.error} />
                </ListItemButton>
              ))}
            </List>
          )}
        </Paper>

        <Paper variant="outlined" sx={{ flexGrow: 1, minWidth: 0, width: "100%", p: 2 }}>
          {selectedReceiver ? (
            <>
              <Stack
                direction="row"
                spacing={1}
                sx={{ alignItems: "center", justifyContent: "space-between", mb: 2 }}
              >
                <Box sx={{ minWidth: 0 }}>
                  <Typography variant="h6" sx={{ overflowWrap: "anywhere" }}>
                    {selectedReceiver.id}
                  </Typography>
                  <Typography
                    variant="body2"
                    color="text.secondary"
                    sx={{ overflowWrap: "anywhere" }}
                  >
                    {selectedReceiver.path}
                  </Typography>
                </Box>
                <Tooltip title="Reload files">
                  <IconButton onClick={() => setReloadKey((k) => k + 1)} aria-label="reload files">
                    <RefreshIcon />
                  </IconButton>
                </Tooltip>
              </Stack>
              <ReceiverFilesTable
                id={selectedReceiver.id}
                canDownload={session.canDownload}
                reloadKey={reloadKey}
              />
            </>
          ) : receivers.loaded && sorted.length ? (
            <Typography color="text.secondary">unknown receiver {selected}</Typography>
          ) : (
            <Typography color="text.secondary">select a receiver</Typography>
          )}
        </Paper>
      </Stack>
    </>
  );
}
