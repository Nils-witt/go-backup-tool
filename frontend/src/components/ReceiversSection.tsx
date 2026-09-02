import { useEffect, useState } from "react";
import { apiFetch, apiFetchJSON } from "../api/client";
import type { ReceiverFile, ReceiverSnapshot } from "../api/types";
import { Badge } from "./Badge";
import { ConfirmDialog } from "./Dialog";
import { encodePathKey, fmtSize, fmtTime, hasTime } from "../lib/format";

interface ReceiversSectionProps {
  receivers: ReceiverSnapshot[];
  canDownload: boolean;
}

function startDownload(id: string, key: string) {
  const url = "/api/receivers/" + encodeURIComponent(id) + "/download/" + encodePathKey(key);

  apiFetch(url, { method: "POST" })
    .then((r) => r.json())
    .then((data: { ticket: string }) => {
      window.location.href = url + "?ticket=" + encodeURIComponent(data.ticket);
    })
    .catch(() => {});
}

function FileList({ id, canDownload, onDownload }: { id: string; canDownload: boolean; onDownload: (id: string, key: string) => void }) {
  const [files, setFiles] = useState<ReceiverFile[] | null>(null);

  useEffect(() => {
    let cancelled = false;

    apiFetchJSON<ReceiverFile[]>("/api/receivers/" + encodeURIComponent(id) + "/files")
      .then((f) => {
        if (!cancelled) setFiles(f || []);
      })
      .catch(() => {
        if (!cancelled) setFiles([]);
      });

    return () => {
      cancelled = true;
    };
  }, [id]);

  if (files === null) return <p className="meta">loading…</p>;
  if (!files.length) return <p className="meta">no files stored</p>;

  return (
    <ul className="files">
      {files.map((f) => (
        <li key={f.key}>
          <span className="file-key">{f.key}</span>
          <span className="file-meta">
            {fmtSize(f.size)} · {fmtTime(f.mod_time)}
            {canDownload ? (
              <>
                {" · "}
                <a
                  href="#"
                  className="download-link"
                  onClick={(e) => {
                    e.preventDefault();
                    onDownload(id, f.key);
                  }}
                >
                  download
                </a>
              </>
            ) : null}
          </span>
        </li>
      ))}
    </ul>
  );
}

export function ReceiversSection({ receivers, canDownload }: ReceiversSectionProps) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [pendingDownload, setPendingDownload] = useState<{ id: string; key: string } | null>(null);

  if (!receivers.length) return null;

  return (
    <div id="receivers-wrap">
      <h2 className="section-title">Receivers</h2>
      <div className="grid" id="receivers">
        {receivers.map((rcv) => {
          const retention = rcv.retention ? " · retention " + rcv.retention : "";
          const staleAfter = rcv.stale_after ? " · stale after " + rcv.stale_after : "";
          const lastSeen = hasTime(rcv.last_seen)
            ? "last received: " + fmtTime(rcv.last_seen) + (rcv.last_key ? " (" + rcv.last_key + ")" : "")
            : "no objects received yet";
          const isExpanded = !!expanded[rcv.id];

          return (
            <div className="card" key={rcv.id}>
              <div className="card-head">
                <span className="job-name">{rcv.id}</span>
                <span className="badges">
                  <Badge state={rcv.state} />
                  {rcv.stale ? <Badge state="failed" label="stale" /> : null}
                </span>
              </div>
              <div className="meta">
                {rcv.path}
                {retention}
                {staleAfter}
              </div>
              <div className="meta">{lastSeen}</div>
              {rcv.error ? <p className="err">{rcv.error}</p> : null}
              <button
                type="button"
                className="files-toggle"
                onClick={() => setExpanded((prev) => ({ ...prev, [rcv.id]: !prev[rcv.id] }))}
              >
                {isExpanded ? "Hide files" : "Show files"}
              </button>
              {isExpanded ? (
                <div className="files-wrap">
                  <FileList
                    id={rcv.id}
                    canDownload={canDownload}
                    onDownload={(id, key) => setPendingDownload({ id, key })}
                  />
                </div>
              ) : null}
            </div>
          );
        })}
      </div>

      <ConfirmDialog
        open={pendingDownload !== null}
        message={
          <>
            Download <strong>{pendingDownload?.key}</strong>?
          </>
        }
        confirmLabel="Download"
        onConfirm={() => {
          if (pendingDownload) startDownload(pendingDownload.id, pendingDownload.key);
          setPendingDownload(null);
        }}
        onCancel={() => setPendingDownload(null)}
      />
    </div>
  );
}
