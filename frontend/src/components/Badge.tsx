export function Badge({ state, label }: { state: string; label?: string }) {
  return <span className={`badge ${state}`}>{label ?? state}</span>;
}
