import { useCallback, useEffect, useState } from "react";
import { apiFetchJSON } from "../api/client";
import type { JobRunEventJSON, JobSnapshot, ReceiverSnapshot, TargetRunEventJSON } from "../api/types";

export interface StatusPollState {
  jobs: JobSnapshot[];
  receivers: ReceiverSnapshot[];
  logs: string[];
  jobRuns: JobRunEventJSON[];
  targetRuns: TargetRunEventJSON[];
  updatedText: string;
  // refreshNow re-polls immediately (used after a mutation like "retry
  // failed targets" so its effect shows up right away, rather than waiting
  // up to 2s for the next scheduled poll) without resetting the interval's
  // own schedule — matching startRetry's refresh() call in dashboard.js.
  refreshNow: () => void;
}

// useStatusPoll ports refresh()'s Promise.all group (dashboard.js:1052-1079):
// the five endpoints polled together every 2s, and the "updated HH:MM:SS"/
// error text shown under the page title.
export function useStatusPoll(): StatusPollState {
  const [jobs, setJobs] = useState<JobSnapshot[]>([]);
  const [receivers, setReceivers] = useState<ReceiverSnapshot[]>([]);
  const [logs, setLogs] = useState<string[]>([]);
  const [jobRuns, setJobRuns] = useState<JobRunEventJSON[]>([]);
  const [targetRuns, setTargetRuns] = useState<TargetRunEventJSON[]>([]);
  const [updatedText, setUpdatedText] = useState("loading…");

  const refresh = useCallback(() => {
    Promise.all([
      apiFetchJSON<JobSnapshot[]>("/api/status"),
      apiFetchJSON<ReceiverSnapshot[]>("/api/receivers"),
      apiFetchJSON<string[]>("/api/logs"),
      apiFetchJSON<JobRunEventJSON[]>("/api/job-runs"),
      apiFetchJSON<TargetRunEventJSON[]>("/api/target-runs"),
    ])
      .then(([jobsRes, receiversRes, logsRes, jobRunsRes, targetRunsRes]) => {
        setJobs(jobsRes || []);
        setReceivers(receiversRes || []);
        setLogs(logsRes || []);
        setJobRuns(jobRunsRes || []);
        setTargetRuns(targetRunsRes || []);
        setUpdatedText("updated " + new Date().toLocaleTimeString());
      })
      .catch((err: unknown) => {
        setUpdatedText("error fetching status: " + String(err));
      });
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 2000);
    return () => clearInterval(id);
  }, [refresh]);

  return { jobs, receivers, logs, jobRuns, targetRuns, updatedText, refreshNow: refresh };
}
