import type { ReactNode } from "react";
import Box from "@mui/material/Box";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Typography from "@mui/material/Typography";
import { useMeta } from "../hooks/useMeta";
import { Footer } from "./Footer";

// AuthCard is the centered single-card layout shared by the login page and
// the SSO callback page, both rendered outside the dashboard Layout.
export function AuthCard({ children }: { children: ReactNode }) {
  const meta = useMeta();

  return (
    <Box sx={{ minHeight: "100vh", display: "flex", flexDirection: "column" }}>
      <Box
        sx={{ flexGrow: 1, display: "flex", alignItems: "center", justifyContent: "center", p: 2 }}
      >
        <Card sx={{ width: "100%", maxWidth: 380 }}>
          <CardContent sx={{ display: "flex", flexDirection: "column", gap: 2 }}>
            <Box>
              <Typography variant="h6" component="h1">
                go-backup-tool
              </Typography>
              {meta?.instanceName ? (
                <Typography variant="subtitle2" color="text.secondary">
                  {meta.instanceName}
                </Typography>
              ) : null}
            </Box>
            {children}
          </CardContent>
        </Card>
      </Box>
      <Footer />
    </Box>
  );
}
