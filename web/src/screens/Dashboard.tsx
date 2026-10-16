import { useEffect, useRef, useState } from "react";
import { api, metricSum } from "../api";
import { Panel, usePoll } from "../components/ui";
import { enumName } from "../fix/parser";
import { stream } from "../fix/stream";
import { go, href } from "../router";
import { useStore } from "../store";

export function Dashboard() {
  const sessions = usePoll(api.sessions, 2000).data ?? [];
  const metrics = usePoll(api.metrics, 2000).data ?? "";
  const reports = useStore(stream.reports);

  // Messages per second from the counter delta between two metric polls
  // (other renders, e.g. new drop copies, must not move the baseline).
  const prev = useRef<{ t: number; n: number }>(undefined);
  const [rate, setRate] = useState(0);
  useEffect(() => {
    if (!metrics) return;
    const now = Date.now();
    const total = metricSum(metrics, "mockfix_fix_messages_total");
    if (prev.current && total >= prev.current.n) setRate((total - prev.current.n) / ((now - prev.current.t) / 1000));
    prev.current = { t: now, n: total };
  }, [metrics]);

  const clients = sessions.filter((s) => s.kind === "client");
  const recent = reports.filter((r) => r.msgType === "8").slice(-15).reverse();

  return (
    <div className="grid">
      <div className="grid cols-4">
        <Stat label="Client sessions logged on" value={`${clients.filter((s) => s.logged_on).length} / ${clients.length}`} />
        <Stat label="Market data subscriptions" value={metricSum(metrics, "mockfix_md_subscriptions")} />
        <Stat label="FIX messages / second" value={rate.toFixed(1)} />
        <Stat label="Orders closed" value={metricSum(metrics, "mockfix_orders_total")} />
      </div>
      <div className="grid cols-2">
        <Panel title="Sessions">
          <table>
            <thead>
              <tr>
                <th>Session</th>
                <th>State</th>
                <th className="num">Seq out / in</th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr key={s.id} className="clickable" onClick={() => go("sessions")}>
                  <td className="mono">{s.target_comp_id}</td>
                  <td>
                    <span className={`dot ${s.logged_on ? "open" : "closed"}`} />
                    {s.logged_on ? "logged on" : "offline"}
                  </td>
                  <td className="num">{s.logged_on ? `${s.next_sender_seq} / ${s.next_target_seq}` : "–"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
        <Panel title="Recent orders (drop copy)" actions={<a href={href("orders")}>All orders</a>}>
          {recent.length === 0 && <p className="muted">No ExecutionReports since this page opened.</p>}
          <table>
            <tbody>
              {recent.map((r, i) => (
                <tr key={i} className="clickable" onClick={() => go("orders", r.get(37) ?? "")}>
                  <td className="mono">{r.get(5001)?.split("->")[1]}</td>
                  <td className="mono">{r.get(11)}</td>
                  <td>{r.get(55)}</td>
                  <td className={r.get(54) === "1" ? "buy" : "sell"}>{enumName(54, r.get(54) ?? "")}</td>
                  <td>{enumName(150, r.get(150) ?? "")}</td>
                  <td className="num mono">{r.get(150) === "F" ? `${r.get(32)} @ ${r.get(31)}` : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      </div>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="panel stat">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
    </div>
  );
}
