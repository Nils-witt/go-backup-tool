import { useState } from "react";
import type { JobRunEventJSON } from "../api/types";
import { Badge } from "./Badge";
import { fmtDuration, fmtSize, fmtTime } from "../lib/format";

export function JobRunLogSection({ events }: { events: JobRunEventJSON[] }) {
  const [job, setJob] = useState("");
  const [result, setResult] = useState("");

  if (!events.length) return null;

  const jobFilter = job.trim().toLowerCase();
  const filtered = events.filter((ev) => {
    if (jobFilter && ev.job_name.toLowerCase().indexOf(jobFilter) === -1) return false;
    if (result === "success" && !ev.success) return false;
    if (result === "failed" && ev.success) return false;
    return true;
  });

  return (
    <div id="job-run-log-wrap">
      <h2 className="section-title">Job run log</h2>
      <div className="card job-run-log-card">
        <div className="log-filters">
          <input
            type="text"
            placeholder="Filter by job…"
            value={job}
            onChange={(e) => setJob(e.target.value)}
          />
          <select value={result} onChange={(e) => setResult(e.target.value)}>
            <option value="">All results</option>
            <option value="success">Success</option>
            <option value="failed">Failed</option>
          </select>
        </div>
        <table className="job-run-events">
          <thead>
            <tr>
              <th>Time</th>
              <th>Job</th>
              <th>Duration</th>
              <th>Size</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 ? (
              <tr>
                <td className="empty-filter" colSpan={5}>
                  No matching runs
                </td>
              </tr>
            ) : (
              filtered.map((ev, i) => (
                <tr key={i}>
                  <td className="nowrap">{fmtTime(ev.end)}</td>
                  <td>{ev.job_name}</td>
                  <td className="nowrap">{fmtDuration(new Date(ev.end).getTime() - new Date(ev.start).getTime())}</td>
                  <td className="nowrap">{fmtSize(ev.size)}</td>
                  <td className="nowrap">
                    {ev.success ? <Badge state="ok" label="success" /> : <Badge state="failed" label="failed" />}
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
