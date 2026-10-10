import Box from "@mui/material/Box";
import Link from "@mui/material/Link";
import { useMeta } from "../hooks/useMeta";

// The copyright year, read once at load rather than on every render.
const year = new Date().getFullYear();

// Footer is the page footer shared by the dashboard layout and the
// (unauthenticated) login pages.
export function Footer() {
  const meta = useMeta();

  return (
    <Box
      component="footer"
      sx={{ py: 2, textAlign: "center", color: "text.secondary", fontSize: ".78rem" }}
    >
      &copy; {year} Witt, Nils · Backup-Tool ·{" "}
      <Link href="https://github.com/Nils-witt/go-backup-tool" color="inherit">
        GitHub
      </Link>{" "}
      · {meta?.version ?? ""} · {meta?.commit ?? ""}
    </Box>
  );
}
