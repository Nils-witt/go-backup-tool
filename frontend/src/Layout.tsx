import { useState } from "react";
import { NavLink, Outlet } from "react-router-dom";
import AppBar from "@mui/material/AppBar";
import Box from "@mui/material/Box";
import Container from "@mui/material/Container";
import Drawer from "@mui/material/Drawer";
import IconButton from "@mui/material/IconButton";
import Link from "@mui/material/Link";
import List from "@mui/material/List";
import ListItemButton from "@mui/material/ListItemButton";
import ListItemIcon from "@mui/material/ListItemIcon";
import ListItemText from "@mui/material/ListItemText";
import ListSubheader from "@mui/material/ListSubheader";
import MenuIcon from "@mui/icons-material/Menu";
import LogoutIcon from "@mui/icons-material/Logout";
import Toolbar from "@mui/material/Toolbar";
import Typography from "@mui/material/Typography";
import Button from "@mui/material/Button";
import { logout } from "./api/client";
import { useMeta } from "./hooks/useMeta";
import { useSession } from "./context/SessionContext";
import { NAV_GROUPS } from "./nav";

const DRAWER_WIDTH = 240;

export function Layout() {
  const meta = useMeta();
  const session = useSession();
  const [mobileOpen, setMobileOpen] = useState(false);

  const drawerContent = (
    <div>
      <Toolbar />
      {NAV_GROUPS.map((group, i) => {
        const items = group.items.filter((item) => !item.visible || item.visible(session));
        if (!items.length) return null;

        return (
          <List
            key={i}
            dense
            subheader={
              group.label ? <ListSubheader component="div">{group.label}</ListSubheader> : undefined
            }
          >
            {items.map((item) => (
              <ListItemButton
                key={item.to}
                component={NavLink}
                to={item.to}
                end={item.to === "/"}
                onClick={() => setMobileOpen(false)}
                sx={{
                  "&.active": {
                    bgcolor: "action.selected",
                    "& .MuiListItemIcon-root": { color: "primary.main" },
                  },
                }}
              >
                <ListItemIcon>{item.icon}</ListItemIcon>
                <ListItemText primary={item.label} />
              </ListItemButton>
            ))}
          </List>
        );
      })}
    </div>
  );

  return (
    <Box sx={{ display: "flex" }}>
      <AppBar position="fixed" sx={{ zIndex: (theme) => theme.zIndex.drawer + 1 }}>
        <Toolbar sx={{ gap: 1 }}>
          <IconButton
            color="inherit"
            edge="start"
            onClick={() => setMobileOpen((v) => !v)}
            sx={{ display: { sm: "none" } }}
          >
            <MenuIcon />
          </IconButton>
          <Typography variant="h6" noWrap component="div" sx={{ flexGrow: 1 }}>
            go-backup-tool
          </Typography>
          {meta?.auth_enabled ? (
            <Button color="inherit" startIcon={<LogoutIcon />} onClick={() => logout()}>
              Log out
            </Button>
          ) : null}
        </Toolbar>
      </AppBar>

      <Box component="nav" sx={{ width: { sm: DRAWER_WIDTH }, flexShrink: { sm: 0 } }}>
        <Drawer
          variant="temporary"
          open={mobileOpen}
          onClose={() => setMobileOpen(false)}
          ModalProps={{ keepMounted: true }}
          sx={{
            display: { xs: "block", sm: "none" },
            "& .MuiDrawer-paper": { boxSizing: "border-box", width: DRAWER_WIDTH },
          }}
        >
          {drawerContent}
        </Drawer>
        <Drawer
          variant="permanent"
          sx={{
            display: { xs: "none", sm: "block" },
            "& .MuiDrawer-paper": { boxSizing: "border-box", width: DRAWER_WIDTH },
          }}
          open
        >
          {drawerContent}
        </Drawer>
      </Box>

      <Box component="main" sx={{ flexGrow: 1, width: { sm: `calc(100% - ${DRAWER_WIDTH}px)` } }}>
        <Toolbar />
        <Container sx={{ py: 3 }}>
          <Outlet />
        </Container>
        <Box
          component="footer"
          sx={{ py: 2, textAlign: "center", color: "text.secondary", fontSize: ".78rem" }}
        >
          &copy; {new Date().getFullYear()} Witt, Nils · Backup-Tool ·{" "}
          <Link href="https://github.com/Nils-witt/go-backup-tool" color="inherit">
            GitHub
          </Link>{" "}
          · {meta?.version ?? ""} · {meta?.commit ?? ""}
        </Box>
      </Box>
    </Box>
  );
}
