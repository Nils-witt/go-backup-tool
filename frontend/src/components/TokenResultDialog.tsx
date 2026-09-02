import { Dialog } from "./Dialog";
import { fmtTime } from "../lib/format";

export interface TokenResult {
  username: string;
  expiresAt: string;
  token: string;
  error: string;
}

// TokenResultDialog shows a freshly issued API token exactly once — the
// server never shows it again — mirroring token-result-dialog in
// dashboard.html.
export function TokenResultDialog({ result, onClose }: { result: TokenResult | null; onClose: () => void }) {
  function copy() {
    if (!result?.token || !navigator.clipboard?.writeText) return;
    navigator.clipboard.writeText(result.token).catch(() => {});
  }

  return (
    <Dialog open={result !== null} onClose={() => onClose()}>
      <form method="dialog">
        <p className="confirm-message">
          Token for <strong>{result?.username}</strong>
          {result?.expiresAt ? <>, valid until {fmtTime(result.expiresAt)}</> : null}. Copy it now — it won't be shown
          again.
        </p>
        <textarea className="pubkey token-result-value" readOnly rows={4} value={result?.token ?? ""} />
        {result?.error ? <p className="err">{result.error}</p> : null}
        <div className="confirm-actions">
          <button type="button" className="btn btn-secondary" onClick={copy}>
            Copy
          </button>
          <button type="submit" value="ok" className="btn btn-primary" autoFocus>
            Close
          </button>
        </div>
      </form>
    </Dialog>
  );
}
