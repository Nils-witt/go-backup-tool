// Formatting helpers ported from dashboard.js.

// The Go zero time.Time serializes as "0001-01-01T00:00:00Z" rather than an
// empty/omitted field.
export function hasTime(s?: string | null): boolean {
  return !!s && new Date(s).getUTCFullYear() > 1;
}

export function fmtTime(s?: string | null): string {
  if (!hasTime(s)) return "never";
  return new Date(s as string).toLocaleString();
}

export function fmtSize(bytes: number): string {
  if (bytes < 1024) return bytes + " B";

  const units = ["KB", "MB", "GB", "TB"];
  let val = bytes;
  let i = -1;

  do {
    val /= 1024;
    i++;
  } while (val >= 1024 && i < units.length - 1);

  return val.toFixed(1) + " " + units[i];
}

// fmtDuration formats a millisecond duration the way a human would read it:
// whole seconds once it's a second or more, milliseconds below that.
export function fmtDuration(ms: number): string {
  if (ms < 1000) return ms + "ms";
  return (ms / 1000).toFixed(1) + "s";
}

// encodePathKey percent-encodes each segment of a "/"-separated file key on
// its own, so a download URL keeps the key's directory structure (matching
// the server's {key...} wildcard route) while still escaping anything else
// that needs it.
export function encodePathKey(key: string): string {
  return key
    .split("/")
    .map((seg) => encodeURIComponent(seg))
    .join("/");
}
