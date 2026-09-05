import type { ReactNode } from "react";
import TableCell from "@mui/material/TableCell";
import TableSortLabel from "@mui/material/TableSortLabel";
import type { SortDirection } from "../hooks/useSortedRows";

// SortableHeaderCell is one <TableCell> in a log table's header, clickable
// to sort that column via useSortedRows — shared by every log table section
// so the click target/arrow indicator look and behave identically everywhere.
export function SortableHeaderCell<K extends string>({
  label,
  sortKey,
  activeKey,
  direction,
  onSort,
  whiteSpace,
}: {
  label: ReactNode;
  sortKey: K;
  activeKey: K;
  direction: SortDirection;
  onSort: (key: K) => void;
  whiteSpace?: boolean;
}) {
  const active = activeKey === sortKey;

  return (
    <TableCell
      sortDirection={active ? direction : false}
      sx={whiteSpace ? { whiteSpace: "nowrap" } : undefined}
    >
      <TableSortLabel
        active={active}
        direction={active ? direction : "asc"}
        onClick={() => onSort(sortKey)}
      >
        {label}
      </TableSortLabel>
    </TableCell>
  );
}
