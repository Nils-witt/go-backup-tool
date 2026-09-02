import { useState } from "react";
import type { TargetRunEventJSON } from "../api/types";
import { Badge } from "./Badge";
import { fmtTime } from "../lib/format";

export function TargetRunLogSection({ events }: { events: TargetRunEventJSON[] }) {
  const [job, setJob] = useState("");
  const [target, setTarget] = useState("");
  const [result, setResult] = useState("");

  if (!events.length) return null;

  const jobFilter = job.trim().toLowerCase();
  const targetFilter = target.trim().toLowerCase();
  const filtered = events.filter((ev) => {
    if (jobFilter && ev.job_name.toLowerCase().indexOf(jobFilter) === -1) return false;
    if (targetFilter && ev.target.toLowerCase().indexOf(targetFilter) === -1) return false;
    if (result === "success" && !ev.success) return false;
    if (result === "failed" && ev.success) return false;
    return true;
  });

  return (
    <div id="target-run-log-wrap">
      <h2 className="section-title">Target run log</h2>
      <div className="card target-run-log-card">
        <div className="log-filters">
          <input
            type="text"
            placeholder="Filter by job…"
            value={job}
            onChange={(e) => setJob(e.target.value)}
          />
          <input
            type="text"
            placeholder="Filter by target…"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
          />
          <select value={result} onChange={(e) => setResult(e.target.value)}>
            <option value="">All results</option>
            <option value="success">Success</option>
            <option value="failed">Failed</option>
          </select>
        </div>
        <table className="target-run-events">
          <thead>
            <tr>
              <th>Time</th>
              <th>Job</th>
              <th>Target</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 ? (
              <tr>
                <td className="empty-filter" colSpan={4}>
                  No matching runs
                </td>
              </tr>
            ) : (
              filtered.map((ev, i) => (
                <tr key={i}>
                  <td className="nowrap">{fmtTime(ev.at)}</td>
                  <td>{ev.job_name}</td>
                  <td>{ev.target}</td>
                  <td className="nowrap">
                    <Badge state={ev.state} />
                    {ev.error ? <p className="err">{ev.error}</p> : null}
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
