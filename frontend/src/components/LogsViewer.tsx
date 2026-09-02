import { useEffect, useRef, useState } from "react";
import Box from "@mui/material/Box";
import Card from "@mui/material/Card";
import CardContent from "@mui/material/CardContent";
import Checkbox from "@mui/material/Checkbox";
import FormControlLabel from "@mui/material/FormControlLabel";
import Stack from "@mui/material/Stack";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

interface LogsViewerProps {
  lines: string[];
}

// LogsViewer preserves the reader's scroll position across refreshes unless
// "Follow" is checked and they're already at (or near) the bottom, so a poll
// landing mid-read doesn't yank the view down.
export function LogsViewer({ lines }: LogsViewerProps) {
  const [follow, setFollow] = useState(true);
  const preRef = useRef<HTMLPreElement>(null);
  const theme = useTheme();

  useEffect(() => {
    const pre = preRef.current;
    if (!pre) return;

    const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 4;
    if (follow && atBottom) {
      pre.scrollTop = pre.scrollHeight;
    }
  }, [lines, follow]);

  if (!lines || !lines.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no log output yet
      </Typography>
    );
  }

  return (
    <Card variant="outlined">
      <CardContent>
        <Stack
          direction="row"
          spacing={1}
          sx={{ alignItems: "baseline", justifyContent: "space-between", mb: 1 }}
        >
          <Typography sx={{ fontWeight: 600 }}>Recent output</Typography>
          <FormControlLabel
            control={
              <Checkbox
                size="small"
                checked={follow}
                onChange={(e) => setFollow(e.target.checked)}
              />
            }
            label="Follow"
            sx={{ color: "text.secondary", m: 0 }}
          />
        </Stack>
        <Box
          component="pre"
          ref={preRef}
          sx={{
            m: 0,
            maxHeight: 480,
            overflow: "auto",
            p: 1.5,
            bgcolor: theme.palette.mode === "dark" ? "grey.900" : "grey.100",
            border: 1,
            borderColor: "divider",
            borderRadius: 1,
            fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
            fontSize: 12,
            lineHeight: 1.6,
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
          }}
        >
          {lines.map((line, i) => {
            let color: string | undefined;
            if (line.indexOf("level=ERROR") !== -1) color = theme.palette.status.failed;
            else if (line.indexOf("level=WARN") !== -1) color = theme.palette.status.running;

            return (
              <Box component="span" key={i} sx={color ? { color } : undefined}>
                {line}
                {i < lines.length - 1 ? "\n" : ""}
              </Box>
            );
          })}
        </Box>
      </CardContent>
    </Card>
  );
}
