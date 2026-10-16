import { api, HttpError } from "../api";
import { Panel, usePoll } from "../components/ui";
import { notify } from "../state/config";

export function Sessions() {
  const { data: sessions = [], error } = usePoll(api.sessions, 2000);

  const run = async (what: string, f: () => Promise<unknown>) => {
    try {
      await f();
      notify("ok", what);
    } catch (e) {
      notify("error", e instanceof HttpError ? e.body.message : String(e));
    }
  };

  return (
    <Panel title="Sessions">
      {error && <p className="err-text">{error}</p>}
      <table>
        <thead>
          <tr>
            <th>Kind</th>
            <th>SenderCompID</th>
            <th>TargetCompID</th>
            <th>State</th>
            <th>Remote</th>
            <th className="num">Next seq out</th>
            <th className="num">Next seq in</th>
            <th>Configured faults</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id}>
              <td>{s.kind}</td>
              <td className="mono">{s.sender_comp_id}</td>
              <td className="mono">{s.target_comp_id}</td>
              <td>
                <span className={`dot ${s.logged_on ? "open" : "closed"}`} />
                {s.logged_on ? "logged on" : "offline"}
              </td>
              <td className="mono">{s.remote ?? ""}</td>
              <td className="num">{s.next_sender_seq ?? "–"}</td>
              <td className="num">{s.next_target_seq ?? "–"}</td>
              <td className="mono">{s.faults ? JSON.stringify(s.faults) : ""}</td>
              <td className="row">
                {s.kind === "client" && (
                  <>
                    <button className="runtime" disabled={!s.logged_on} onClick={() => void run(`Logout sent to ${s.target_comp_id}`, () => api.logout(s.target_comp_id))}>
                      Logout
                    </button>
                    <button className="runtime" disabled={!s.logged_on} title="Close the TCP connection now" onClick={() => void run(`${s.target_comp_id}: connection dropped`, () => api.fault(s.target_comp_id, "drop"))}>
                      Drop
                    </button>
                    <button className="runtime" disabled={!s.logged_on} title="Skip one MsgSeqNum; the client must send ResendRequest" onClick={() => void run(`${s.target_comp_id}: next message skips a sequence number`, () => api.fault(s.target_comp_id, "seq_gap"))}>
                      Seq gap
                    </button>
                    <button className="runtime" disabled={!s.logged_on} title="Stop heartbeats until the session reconnects" onClick={() => void run(`${s.target_comp_id}: heartbeats stopped`, () => api.fault(s.target_comp_id, "heartbeat_miss"))}>
                      Stop heartbeats
                    </button>
                  </>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  );
}
