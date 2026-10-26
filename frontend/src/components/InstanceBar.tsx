import { useState } from "react";
import type { RunState } from "../api/types";
import Button from "@mui/material/Button";
import Chip from "@mui/material/Chip";
import ErrorOutlineIcon from "@mui/icons-material/ErrorOutlineOutlined";
import VisibilityOffOutlinedIcon from "@mui/icons-material/VisibilityOffOutlined";
import VisibilityOutlinedIcon from "@mui/icons-material/VisibilityOutlined";
import Popover from "@mui/material/Popover";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { alpha, useTheme } from "@mui/material/styles";

export interface Instance {
  id: string;
  name: string;
  state: RunState;
  // error explains a failed state, e.g. an instance that stopped pushing.
  error?: string;
  // onForget, when set, offers to drop the instance from the dashboard.
  onForget?: () => void;
}

// InstanceChip shows one instance's state, so an instance that stopped
// pushing its status is visible rather than its jobs silently going stale.
// Clicking it shows or hides the instance's jobs and receivers; an error
// icon opens the error text (and, where allowed, a way to forget it).
function InstanceChip({
  instance,
  shown,
  onToggle,
}: {
  instance: Instance;
  shown: boolean;
  onToggle: () => void;
}) {
  const theme = useTheme();
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const color = shown ? theme.palette.status[instance.state] : theme.palette.text.disabled;
  const tint = (a: number) => alpha(color, theme.palette.mode === "dark" ? a + 0.06 : a);
  const VisibilityIcon = shown ? VisibilityOutlinedIcon : VisibilityOffOutlinedIcon;

  return (
    <>
      <Chip
        size="small"
        label={instance.name}
        icon={<VisibilityIcon />}
        variant={shown ? "filled" : "outlined"}
        onClick={onToggle}
        aria-pressed={shown}
        aria-label={`${shown ? "Hide" : "Show"} ${instance.name}`}
        onDelete={instance.error ? (e) => setAnchor(e.currentTarget.parentElement) : undefined}
        deleteIcon={
          instance.error ? (
            <ErrorOutlineIcon aria-label={`${instance.name}: show error details`} />
          ) : undefined
        }
        sx={{
          color,
          borderColor: color,
          backgroundColor: shown ? tint(0.12) : "transparent",
          fontWeight: 600,
          fontSize: ".72rem",
          textTransform: "uppercase",
          letterSpacing: ".02em",
          textDecoration: shown ? undefined : "line-through",
          "& .MuiChip-icon, & .MuiChip-deleteIcon": { color, fontSize: "1rem" },
          "& .MuiChip-deleteIcon:hover": { color },
          "&.MuiChip-clickable:hover": { backgroundColor: tint(0.2) },
        }}
      />
      {instance.error ? (
        <Popover
          open={anchor !== null}
          anchorEl={anchor}
          onClose={() => setAnchor(null)}
          anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
          transformOrigin={{ vertical: "top", horizontal: "right" }}
        >
          <Stack spacing={1} sx={{ p: 1.5, maxWidth: 400 }}>
            <Typography
              variant="body2"
              color="error"
              sx={{ overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}
            >
              {instance.error}
            </Typography>
            {instance.onForget ? (
              <Button
                size="small"
                variant="outlined"
                color="error"
                sx={{ alignSelf: "flex-end" }}
                onClick={() => {
                  setAnchor(null);
                  instance.onForget?.();
                }}
              >
                Forget instance
              </Button>
            ) : null}
          </Stack>
        </Popover>
      ) : null}
    </>
  );
}

// InstanceBar lists every instance in view as an InstanceChip. At least one
// stays shown: hiding the last is ignored.
export function InstanceBar({
  instances,
  hidden,
  setHidden,
}: {
  instances: Instance[];
  hidden: string[];
  setHidden: (ids: string[]) => void;
}) {
  const shownCount = instances.filter((i) => !hidden.includes(i.id)).length;
  const toggle = (id: string) => {
    if (hidden.includes(id)) setHidden(hidden.filter((h) => h !== id));
    // Ids of instances no longer in view stay in the list, so one that
    // comes back keeps whether it's hidden.
    else if (shownCount > 1) setHidden([...hidden, id]);
  };

  return (
    <Stack direction="row" spacing={1} useFlexGap sx={{ flexWrap: "wrap", mb: 2 }}>
      {instances.map((i) => (
        <InstanceChip
          key={i.id}
          instance={i}
          shown={!hidden.includes(i.id)}
          onToggle={() => toggle(i.id)}
        />
      ))}
    </Stack>
  );
}
