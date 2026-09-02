import { useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { IdentityJSON } from "../api/types";
import Box from "@mui/material/Box";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

// IdentitySection fetches /api/identity once (this data never changes while
// the process is running).
export function IdentitySection() {
  const [identity, setIdentity] = useState<IdentityJSON | null>(null);
  const theme = useTheme();

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
        <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: "anywhere" }}>
          UUID: <Box component="code">{identity.uuid}</Box>
        </Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1 }}>
          Public key — paste into a receiving instance's <code>receivers:</code> entry as{" "}
          <code>public-key:</code>:
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
