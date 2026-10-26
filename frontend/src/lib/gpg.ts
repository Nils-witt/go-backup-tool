import type { GPGKeyJSON } from "../api/types";

// keyState summarizes whether a job can encrypt to k.
export function keyState(k: GPGKeyJSON): { state: string; label: string } {
  if (k.revoked) return { state: "failed", label: "revoked" };
  if (k.expired) return { state: "failed", label: "expired" };
  if (k.disabled) return { state: "failed", label: "disabled" };
  if (!k.can_encrypt) return { state: "incomplete", label: "can't encrypt" };
  return { state: "ok", label: "usable" };
}

// groupFingerprint spaces a fingerprint into groups of four for reading.
export function groupFingerprint(fpr: string): string {
  return fpr.replace(/(.{4})(?=.)/g, "$1 ");
}

// keyEmails is the email address of each of k's user IDs: the part inside
// "<…>", or the whole user ID when it has none.
export function keyEmails(k: GPGKeyJSON): string[] {
  return k.user_ids.map((u) => /<([^>]+)>/.exec(u)?.[1] ?? u);
}

function normalize(s: string): string {
  return s.replace(/\s+/g, "").replace(/^0x/i, "").toUpperCase();
}

// findKey is the key a job recipient refers to: by fingerprint, key ID
// (a fingerprint suffix), or email address. It's undefined when none match.
export function findKey(keys: GPGKeyJSON[], recipient: string): GPGKeyJSON | undefined {
  const r = normalize(recipient);
  if (!r) return undefined;
  return keys.find(
    (k) =>
      normalize(k.fingerprint) === r ||
      (r.length >= 8 && normalize(k.fingerprint).endsWith(r)) ||
      normalize(k.key_id) === r ||
      keyEmails(k).some((e) => e.toUpperCase() === r),
  );
}
