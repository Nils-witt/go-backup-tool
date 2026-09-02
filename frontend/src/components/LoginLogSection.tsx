import type { LoginEventJSON } from "../api/types";
import { Badge } from "./Badge";
import { fmtTime } from "../lib/format";

export function LoginLogSection({ events }: { events: LoginEventJSON[] }) {
  if (!events.length) return null;

  return (
    <div id="login-log-wrap">
      <h2 className="section-title">Login log</h2>
      <div className="card login-log-card">
        <table className="login-events">
          <thead>
            <tr>
              <th>Time</th>
              <th>Username</th>
              <th>Method</th>
              <th>Result</th>
              <th>Remote address</th>
            </tr>
          </thead>
          <tbody>
            {events.map((ev, i) => (
              <tr key={i}>
                <td className="nowrap">{fmtTime(ev.at)}</td>
                <td>{ev.username || "(unknown)"}</td>
                <td className="nowrap">{ev.method}</td>
                <td className="nowrap">
                  {ev.success ? <Badge state="ok" label="success" /> : <Badge state="failed" label="failed" />}
                  {ev.detail ? <p className="err">{ev.detail}</p> : null}
                </td>
                <td>{ev.remote_addr}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
