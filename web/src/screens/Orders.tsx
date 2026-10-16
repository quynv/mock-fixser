import { Fragment, useMemo, useState } from "react";
import { api } from "../api";
import { TagTable } from "../components/TagTable";
import { Panel, Select, usePoll } from "../components/ui";
import { enumName, type FixMessage } from "../fix/parser";
import { stream } from "../fix/stream";
import { go } from "../router";
import { useStore } from "../store";

const STATUSES = ["open", "closed", "0", "1", "2", "4", "8", "C"];

export function Orders({ selected }: { selected?: string }) {
  const [session, setSession] = useState("");
  const [symbol, setSymbol] = useState("");
  const [status, setStatus] = useState("");
  const sessions = usePoll(api.sessions, 5000).data ?? [];
  const { data: orders = [] } = usePoll(() => api.orders({ session, symbol, status, limit: "500" }), 2000, [session, symbol, status]);
  const reports = useStore(stream.reports);
  const order = orders.find((o) => o.order_id === selected);

  return (
    <div className="grid">
      <Panel
        title="Orders"
        actions={
          <>
            <Select
              value={session}
              allowEmpty="all sessions"
              options={sessions.filter((s) => s.kind === "client").map((s) => s.target_comp_id)}
              onChange={setSession}
            />
            <input placeholder="symbol" value={symbol} onChange={(e) => setSymbol(e.target.value.toUpperCase())} />
            <select value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">any status</option>
              {STATUSES.map((s) => (
                <option key={s} value={s}>
                  {enumName(39, s) ?? s}
                </option>
              ))}
            </select>
          </>
        }
      >
        <table>
          <thead>
            <tr>
              <th>Created</th>
              <th>OrderID</th>
              <th>Session</th>
              <th>ClOrdID</th>
              <th>List</th>
              <th>Symbol</th>
              <th>Side</th>
              <th className="num">Qty</th>
              <th className="num">Cum</th>
              <th className="num">Leaves</th>
              <th className="num">AvgPx</th>
              <th>Status</th>
              <th>Rule</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((o) => (
              <tr
                key={o.order_id}
                className={`clickable${o.order_id === selected ? " selected" : ""}`}
                onClick={() => go("orders", o.order_id)}
              >
                <td className="mono">{new Date(o.created_at).toLocaleTimeString()}</td>
                <td className="mono">{o.order_id}</td>
                <td className="mono">{o.session.split("->")[1]}</td>
                <td className="mono">{o.cl_ord_id}</td>
                <td className="mono">{o.list_id ?? ""}</td>
                <td>{o.symbol}</td>
                <td className={o.side === "1" ? "buy" : "sell"}>{enumName(54, o.side)}</td>
                <td className="num">{o.order_qty}</td>
                <td className="num">{o.cum_qty}</td>
                <td className="num">{o.leaves_qty}</td>
                <td className="num">{o.avg_px}</td>
                <td>{enumName(39, o.status) ?? o.status}</td>
                <td className="mono">{o.rule}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {orders.length === 0 && <p className="muted">No orders match.</p>}
      </Panel>
      {selected && <OrderChain orderID={selected} reports={reports} known={!!order} />}
    </div>
  );
}

/** Every ExecutionReport of an order seen in the drop copy since the page opened. */
function OrderChain({ orderID, reports, known }: { orderID: string; reports: FixMessage[]; known: boolean }) {
  const chain = useMemo(() => reports.filter((r) => r.msgType === "8" && r.get(37) === orderID), [reports, orderID]);
  const [open, setOpen] = useState<number>();
  return (
    <Panel title={`Message chain of ${orderID}`}>
      {!known && <p className="muted">This order is not in the current filter.</p>}
      {chain.length === 0 && <p className="muted">No ExecutionReports for this order since this page opened (drop copy).</p>}
      <table>
        <tbody>
          {chain.map((r, i) => (
            <Fragment key={i}>
              <tr className="clickable" onClick={() => setOpen(open === i ? undefined : i)}>
                <td className="mono">{r.get(60)}</td>
                <td>{enumName(150, r.get(150) ?? "")}</td>
                <td>{enumName(39, r.get(39) ?? "")}</td>
                <td className="mono">{r.get(11)}</td>
                <td className="num mono">{r.get(150) === "F" ? `${r.get(32)} @ ${r.get(31)}` : ""}</td>
                <td className="num mono">
                  cum {r.get(14)} · leaves {r.get(151)}
                </td>
                <td>{r.get(58)}</td>
              </tr>
              {open === i && (
                <tr>
                  <td colSpan={7}>
                    <TagTable msg={r} />
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </Panel>
  );
}
