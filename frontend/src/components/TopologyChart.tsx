import { memo, useCallback, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { JobSnapshot, ReceiverSnapshot, RunState } from "../api/types";
import {
  buildTopology,
  connectedKeys,
  type TopologyColumn,
  type TopologyNode,
} from "../lib/topology";
import type { Sourced } from "../lib/status";
import Box from "@mui/material/Box";
import Paper from "@mui/material/Paper";
import Stack from "@mui/material/Stack";
import Tooltip from "@mui/material/Tooltip";
import Typography from "@mui/material/Typography";
import { useTheme } from "@mui/material/styles";

const COLUMNS: { column: TopologyColumn; title: string; empty: string }[] = [
  { column: "job", title: "Jobs", empty: "no jobs" },
  { column: "server", title: "Destinations", empty: "no targets" },
  { column: "receiver", title: "Receivers", empty: "no receivers in view" },
];

const LEGEND: RunState[] = ["ok", "running", "incomplete", "failed", "idle"];

// The horizontal gap between columns, which the connecting curves span.
const gutter = 88;

interface Point {
  x: number;
  y: number;
}

interface Anchors {
  width: number;
  height: number;
  // Each node's left (in) and right (out) anchor, relative to the chart.
  in: Record<string, Point>;
  out: Record<string, Point>;
}

// Memoized: hovering re-renders the chart, but only the cards whose dimmed
// flag actually flips need to re-render with it.
const NodeCard = memo(function NodeCard({
  node,
  dimmed,
  onHover,
  nodeRef,
}: {
  node: TopologyNode;
  dimmed: boolean;
  onHover: (key: string | null) => void;
  nodeRef: (el: HTMLElement | null) => (() => void) | undefined;
}) {
  const theme = useTheme();
  const color = theme.palette.status[node.state] ?? theme.palette.status.idle;
  const stateLabel = node.stale ? "stale" : node.state;

  const card = (
    <Paper
      ref={nodeRef}
      data-node-key={node.key}
      variant="outlined"
      tabIndex={0}
      onMouseEnter={() => onHover(node.key)}
      onMouseLeave={() => onHover(null)}
      onFocus={() => onHover(node.key)}
      onBlur={() => onHover(null)}
      sx={{
        px: 1.5,
        py: 1,
        borderLeft: `4px solid ${color}`,
        opacity: dimmed ? 0.35 : 1,
        transition: "opacity .15s",
        outline: "none",
        "&:focus-visible": { boxShadow: `0 0 0 2px ${theme.palette.primary.main}` },
      }}
    >
      <Stack direction="row" spacing={1} sx={{ alignItems: "baseline", minWidth: 0 }}>
        <Typography variant="body2" noWrap sx={{ fontWeight: 600, flex: 1, minWidth: 0 }}>
          {node.label}
        </Typography>
        <Typography
          variant="caption"
          sx={{ color, fontWeight: 600, textTransform: "uppercase", fontSize: ".68rem" }}
        >
          {stateLabel}
        </Typography>
      </Stack>
      <Typography variant="caption" color="text.secondary" noWrap component="div">
        {[node.source, node.detail].filter(Boolean).join(" · ") || " "}
      </Typography>
    </Paper>
  );

  return node.error ? (
    <Tooltip title={node.error} arrow placement="top">
      {card}
    </Tooltip>
  ) : (
    card
  );
});

// sameAnchors reports whether a and b would draw identical curves, so a
// re-measure that moved nothing doesn't trigger another render.
function sameAnchors(a: Anchors | null, b: Anchors): boolean {
  if (!a || a.width !== b.width || a.height !== b.height) return false;
  const keys = Object.keys(b.in);
  if (keys.length !== Object.keys(a.in).length) return false;
  return keys.every((k) => {
    const ai = a.in[k],
      bi = b.in[k],
      ao = a.out[k],
      bo = b.out[k];
    return ai && ao && ai.x === bi.x && ai.y === bi.y && ao.x === bo.x && ao.y === bo.y;
  });
}

// TopologyChart draws which job uploads to which server, and which
// receiver each remote server delivers to, as three columns joined by
// curves colored by the state of the targets they carry. Hovering a node
// highlights every path through it.
export function TopologyChart({
  jobs,
  receivers,
}: {
  jobs: Sourced<JobSnapshot>[];
  receivers: Sourced<ReceiverSnapshot>[];
}) {
  const theme = useTheme();
  const { topology, byColumn, layoutKey } = useMemo(() => {
    const topology = buildTopology(jobs, receivers);
    const byColumn: Record<TopologyColumn, TopologyNode[]> = { job: [], server: [], receiver: [] };
    for (const n of topology.nodes) byColumn[n.column].push(n);
    // Only the set and order of nodes moves them; their states don't.
    const layoutKey = topology.nodes.map((n) => n.key).join("\u0002");
    return { topology, byColumn, layoutKey };
  }, [jobs, receivers]);
  const [hovered, setHovered] = useState<string | null>(null);
  const highlight = useMemo(
    () => (hovered ? connectedKeys(topology, hovered) : null),
    [topology, hovered],
  );

  const containerRef = useRef<HTMLDivElement>(null);
  const nodeEls = useRef(new Map<string, HTMLElement>());
  const [anchors, setAnchors] = useState<Anchors | null>(null);

  // One stable ref callback for every node (keyed by its data-node-key),
  // using React 19's ref cleanup: a fresh callback each render would make
  // React detach and re-attach every node's ref on every hover.
  const registerNode = useCallback((el: HTMLElement | null) => {
    const key = el?.dataset.nodeKey;
    if (!el || !key) return;
    nodeEls.current.set(key, el);
    return () => {
      nodeEls.current.delete(key);
    };
  }, []);

  const measure = useCallback(() => {
    const container = containerRef.current;
    if (!container) return;
    const box = container.getBoundingClientRect();
    const next: Anchors = { width: box.width, height: box.height, in: {}, out: {} };
    for (const [key, el] of nodeEls.current) {
      const r = el.getBoundingClientRect();
      const y = r.top - box.top + r.height / 2;
      next.in[key] = { x: r.left - box.left, y };
      next.out[key] = { x: r.right - box.left, y };
    }
    setAnchors((prev) => (sameAnchors(prev, next) ? prev : next));
  }, []);

  useLayoutEffect(measure, [measure, layoutKey]);
  useLayoutEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    const ro = new ResizeObserver(measure);
    ro.observe(container);
    return () => ro.disconnect();
  }, [measure]);

  if (!topology.nodes.length) {
    return (
      <Typography color="text.secondary" sx={{ mt: 6, textAlign: "center" }}>
        no jobs or receivers configured
      </Typography>
    );
  }

  return (
    <>
      <Box sx={{ overflowX: "auto", pb: 1 }}>
        <Box
          ref={containerRef}
          sx={{
            position: "relative",
            display: "grid",
            gridTemplateColumns: "repeat(3, minmax(180px, 1fr))",
            columnGap: `${gutter}px`,
            minWidth: 3 * 180 + 2 * gutter,
          }}
        >
          {anchors ? (
            <svg
              width={anchors.width}
              height={anchors.height}
              aria-hidden
              style={{ position: "absolute", inset: 0, pointerEvents: "none", overflow: "visible" }}
            >
              {topology.edges.map((e) => {
                const a = anchors.out[e.from];
                const b = anchors.in[e.to];
                if (!a || !b) return null;
                const mid = (a.x + b.x) / 2;
                const lit = highlight?.has(e.key);
                return (
                  <path
                    key={e.key}
                    d={`M${a.x},${a.y} C${mid},${a.y} ${mid},${b.y} ${b.x},${b.y}`}
                    fill="none"
                    stroke={theme.palette.status[e.state] ?? theme.palette.status.idle}
                    strokeWidth={lit ? 3 : 2}
                    strokeDasharray={e.ambiguous ? "5 4" : undefined}
                    strokeLinecap="round"
                    opacity={highlight ? (lit ? 1 : 0.12) : 0.7}
                    style={{ transition: "opacity .15s" }}
                  >
                    <title>{e.label}</title>
                  </path>
                );
              })}
            </svg>
          ) : null}

          {COLUMNS.map(({ column, title, empty }) => {
            const nodes = byColumn[column];
            return (
              <Box key={column} sx={{ minWidth: 0 }}>
                <Typography
                  variant="overline"
                  color="text.secondary"
                  component="div"
                  sx={{ mb: 1, lineHeight: 1.6 }}
                >
                  {title} · {nodes.length}
                </Typography>
                <Stack spacing={1.5}>
                  {nodes.length ? (
                    nodes.map((n) => (
                      <NodeCard
                        key={n.key}
                        node={n}
                        dimmed={highlight !== null && !highlight.has(n.key)}
                        onHover={setHovered}
                        nodeRef={registerNode}
                      />
                    ))
                  ) : (
                    <Typography variant="body2" color="text.secondary">
                      {empty}
                    </Typography>
                  )}
                </Stack>
              </Box>
            );
          })}
        </Box>
      </Box>

      <Stack
        direction="row"
        sx={{ flexWrap: "wrap", columnGap: 2, rowGap: 0.5, mt: 2, alignItems: "center" }}
      >
        {LEGEND.map((s) => (
          <Stack key={s} direction="row" spacing={0.75} sx={{ alignItems: "center" }}>
            <Box sx={{ width: 16, height: 3, borderRadius: 2, bgcolor: theme.palette.status[s] }} />
            <Typography variant="caption" color="text.secondary">
              {s}
            </Typography>
          </Stack>
        ))}
        <Typography variant="caption" color="text.secondary">
          · dashed: receiver id found on several instances · remote targets connect to the receiver
          named by their bucket
        </Typography>
      </Stack>
    </>
  );
}
