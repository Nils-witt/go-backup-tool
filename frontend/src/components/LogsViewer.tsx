import { useEffect, useRef, useState } from "react";

interface LogsViewerProps {
  lines: string[];
}

// LogsViewer ports renderLogs (dashboard.js:317-339): it preserves the
// reader's scroll position across refreshes unless "Follow" is checked and
// they're already at (or near) the bottom, so a poll landing mid-read
// doesn't yank the view down.
export function LogsViewer({ lines }: LogsViewerProps) {
  const [follow, setFollow] = useState(true);
  const preRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const pre = preRef.current;
    if (!pre) return;

    const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 4;
    if (follow && atBottom) {
      pre.scrollTop = pre.scrollHeight;
    }
  }, [lines, follow]);

  if (!lines || !lines.length) return null;

  return (
    <div id="logs-wrap">
      <h2 className="section-title">Logs</h2>
      <div className="card logs-card">
        <div className="card-head">
          <span className="job-name">Recent output</span>
          <label className="follow-toggle">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
          </label>
        </div>
        <pre className="logs" ref={preRef}>
          {lines.map((line, i) => {
            let cls = "";
            if (line.indexOf("level=ERROR") !== -1) cls = "log-error";
            else if (line.indexOf("level=WARN") !== -1) cls = "log-warn";

            return (
              <span className={cls} key={i}>
                {line}
                {i < lines.length - 1 ? "\n" : ""}
              </span>
            );
          })}
        </pre>
      </div>
    </div>
  );
}
