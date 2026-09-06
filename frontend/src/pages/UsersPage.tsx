import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { UsersAdminSection } from "../components/UsersAdminSection";
import { GroupsAdminSection } from "../components/GroupsAdminSection";
import { PageHeader } from "../components/PageHeader";
import { RequirePermission } from "../components/RequirePermission";

export function UsersPage() {
  return (
    <>
      <PageHeader title="Users" subtitle="Web UI accounts, their permissions, and API tokens." />
      <RequirePermission test={(s) => !!s.info?.admin}>
        <Stack spacing={4}>
          <UsersAdminSection />
          <Stack spacing={1.5}>
            <Typography variant="h6">Groups</Typography>
            <Typography variant="body2" color="text.secondary">
              Bundle permissions into a group, then assign users to it from the table above — a
              user's effective permissions are its own plus every group it belongs to.
            </Typography>
            <GroupsAdminSection />
          </Stack>
        </Stack>
      </RequirePermission>
    </>
  );
}
