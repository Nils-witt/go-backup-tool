import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { IdentityJSON } from "../api/types";
import { useMeta } from "../hooks/useMeta";
import Box from "@mui/material/Box";
import Button from "@mui/material/Button";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import DownloadIcon from "@mui/icons-material/Download";
import { useTheme } from "@mui/material/styles";

// exportIdentity saves the identity as a JSON file whose id, name and
// public_key are what the receiving instance's trusted server form asks for.
function exportIdentity(identity: IdentityJSON, instanceName?: string) {
  const name = instanceName || identity.uuid;
  const data = {
    id: identity.uuid,
    name,
    public_key: identity.public_key,
    fingerprint: identity.fingerprint,
  };
  const blob = new Blob([JSON.stringify(data, null, 2) + "\n"], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name.replace(/[^\w.-]+/g, "_") + "-identity.json";
  a.click();
  URL.revokeObjectURL(url);
}

// IdentitySection fetches /api/identity once (this data never changes while
// the process is running).
export function IdentitySection() {
  const [identity, setIdentity] = useState<IdentityJSON | null>(null);
  const theme = useTheme();
  const meta = useMeta();

  useEffect(() => {
    let cancelled = false;

    apiFetchJSON<IdentityJSON>("/api/identity")
      .then((data) => {
        if (!cancelled) setIdentity(data);
      })
      .catch(() => {});

    return () => {
      cancelled = true;
    };
  }, []);

  if (!identity || !identity.uuid) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no server identity available
      </Typography>
    );
  }

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack
          direction="row"
          spacing={2}
          sx={{ alignItems: "flex-start", justifyContent: "space-between" }}
        >
          <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: "anywhere" }}>
            Server ID: <Box component="code">{identity.uuid}</Box>
          </Typography>
          <Button
            size="small"
            variant="outlined"
            startIcon={<DownloadIcon />}
            onClick={() => exportIdentity(identity, meta?.instanceName)}
            sx={{ flexShrink: 0 }}
          >
            Export identity
          </Button>
        </Stack>
        {identity.fingerprint ? (
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ mt: 1, overflowWrap: "anywhere" }}
          >
            Key fingerprint: <Box component="code">{identity.fingerprint}</Box>
          </Typography>
        ) : null}
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
          Public key — add this server ID and key as a trusted server on the receiving instance:
        </Typography>
        <Box
          component="pre"
          sx={{
            mt: 1,
            p: 1.5,
            bgcolor: theme.palette.mode === "dark" ? "grey.900" : "grey.100",
            border: 1,
            borderColor: "divider",
            borderRadius: 1,
            fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
            fontSize: 12,
            whiteSpace: "pre-wrap",
            wordBreak: "break-all",
          }}
        >
          {identity.public_key}
        </Box>
      </CardContent>
    </Card>
  );
}
