import { useState } from "react";
import type { ReceiverSnapshot } from "../api/types";
import { StatusChip } from "./StatusChip";
import { Fact } from "./Fact";
import { ReceiverFilesTable } from "./ReceiverFilesTable";
import { fmtRelative, fmtTime, hasTime } from "../lib/format";
import { sourcedKey, type Sourced } from "../lib/status";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import CloseIcon from "@mui/icons-material/Close";
import Dialog from "@mui/material/Dialog";
import Grid from "@mui/material/Grid";
import IconButton from "@mui/material/IconButton";
import Stack from "@mui/material/Stack";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

interface ReceiversSectionProps {
  // receivers may come from several instances (see Sourced); remote ones
  // offer no file listing: their files are on the other instance.
  receivers: Sourced<ReceiverSnapshot>[];
  canDownload: boolean;
}

function FileListDialog({
  id,
  canDownload,
  onClose,
}: {
  id: string | null;
  canDownload: boolean;
  onClose: () => void;
}) {
  return (
    <Dialog open={id !== null} onClose={onClose} fullScreen>
      <AppBar position="relative" color="default" elevation={1}>
        <Toolbar sx={{ gap: 1 }}>
          <Typography variant="h6" component="div" sx={{ flexGrow: 1 }}>
            Files{id ? " · " + id : ""}
          </Typography>
          <IconButton edge="end" onClick={onClose} aria-label="close">
            <CloseIcon />
          </IconButton>
        </Toolbar>
      </AppBar>
      <Box sx={{ p: 2 }}>
        <ReceiverFilesTable id={id} canDownload={canDownload} />
      </Box>
    </Dialog>
  );
}

export function ReceiversCard({
  receiver,
  canDownload,
}: {
  receiver: Sourced<ReceiverSnapshot>;
  canDownload: boolean;
}) {
  const theme = useTheme();
  // A stale receiver is accented like an incomplete run, matching how the
  // summary bar counts it.
  const accent = theme.palette.status[receiver.stale ? "incomplete" : receiver.state];

  const [openFilesFor, setOpenFilesFor] = useState<string | null>(null);

  return (
    <>
      <Card
        variant="outlined"
        sx={{ height: "100%", borderLeft: 4, borderLeftColor: accent ?? theme.palette.status.idle }}
      >
        <CardContent>
          <Stack
            direction="row"
            spacing={1}
            sx={{ alignItems: "center", justifyContent: "space-between", mb: 1.5 }}
          >
            <Box sx={{ minWidth: 0 }}>
              <Typography sx={{ fontWeight: 600, overflowWrap: "anywhere" }}>
                {receiver.id}
              </Typography>
              {receiver.instance ? (
                <Typography variant="caption" color="text.secondary">
                  {receiver.instance}
                </Typography>
              ) : null}
            </Box>
            <Stack direction="row" spacing={0.5}>
              {receiver.stale ? <StatusChip state="incomplete" label="stale" /> : null}
              <StatusChip state={receiver.state} error={receiver.error} />
            </Stack>
          </Stack>

          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fill, minmax(96px, 1fr))",
              gap: 1,
              mb: 1.5,
            }}
          >
            <Fact
              label="Last received"
              title={hasTime(receiver.last_seen) ? fmtTime(receiver.last_seen) : undefined}
            >
              {fmtRelative(receiver.last_seen)}
            </Fact>
            {receiver.kind === "directory" ? <Fact label="Type">directory</Fact> : null}
            <Fact label="Retention">{receiver.retention || "—"}</Fact>
            {receiver.keep_versions ? (
              <Fact label="Keep versions">{receiver.keep_versions}</Fact>
            ) : null}
            {receiver.stale_after ? <Fact label="Stale after">{receiver.stale_after}</Fact> : null}
          </Box>

          <Typography variant="caption" color="text.secondary">
            Path
          </Typography>
          <Typography variant="body2" sx={{ mt: 0.5, overflowWrap: "anywhere" }}>
            {receiver.path}
          </Typography>

          {receiver.last_key ? (
            <>
              <Typography variant="caption" color="text.secondary" component="div" sx={{ mt: 1 }}>
                Last object
              </Typography>
              <Typography variant="body2" sx={{ mt: 0.5, overflowWrap: "anywhere" }}>
                {receiver.last_key}
              </Typography>
            </>
          ) : null}

          {receiver.remote ? null : (
            <Box sx={{ mt: 1.5 }}>
              <Button size="small" variant="outlined" onClick={() => setOpenFilesFor(receiver.id)}>
                Show files
              </Button>
            </Box>
          )}
        </CardContent>
      </Card>
      <FileListDialog
        id={openFilesFor}
        canDownload={canDownload}
        onClose={() => setOpenFilesFor(null)}
      />
    </>
  );
}

export function ReceiversSection({ receivers, canDownload }: ReceiversSectionProps) {
  if (!receivers.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no receivers configured
      </Typography>
    );
  }

  return (
    <>
      <Grid container spacing={2}>
        {receivers.map((rcv) => {
          return (
            <Grid key={sourcedKey(rcv.instance, rcv.id)} size={{ xs: 12, sm: 6, md: 4 }}>
              <ReceiversCard receiver={rcv} canDownload={canDownload} />
            </Grid>
          );
        })}
      </Grid>
    </>
  );
}
