import { useMemo, useState } from "react";

export type SortDirection = "asc" | "desc";

// useSortedRows sorts rows by whichever of comparators' keys is currently
// selected (defaultKey/defaultDir initially), toggling asc/desc on repeated
// clicks of the same column and resetting to asc on a new one — the usual
// MUI TableSortLabel/DataGrid convention. Shared by every log table section
// (Login/Download/Receiver/JobRuns/TargetRuns) so each only has to supply
// its own column comparators.
export function useSortedRows<T, K extends string>(
  rows: T[],
  comparators: Record<K, (a: T, b: T) => number>,
  defaultKey: K,
  defaultDir: SortDirection = "desc",
) {
  const [sortKey, setSortKey] = useState<K>(defaultKey);
  const [sortDir, setSortDir] = useState<SortDirection>(defaultDir);

  const sorted = useMemo(() => {
    const factor = sortDir === "asc" ? 1 : -1;
    const cmp = comparators[sortKey];
    return [...rows].sort((a, b) => factor * cmp(a, b));
  }, [rows, comparators, sortKey, sortDir]);

  function toggleSort(key: K) {
    if (key === sortKey) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      setSortDir("asc");
    }
  }

  return { sorted, sortKey, sortDir, toggleSort };
}
