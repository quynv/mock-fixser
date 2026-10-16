import { api, HttpError } from "../api";
import { Panel, usePoll } from "../components/ui";
import { notify } from "../state/config";

export function Sessions() {
  const { data: sessions = [], error } = usePoll(api.sessions, 2000);

  const logout = async (id: string) => {
    try {
      await api.logout(id);
      notify("ok", `Logout sent to ${id}`);
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
                    <button className="runtime" disabled={!s.logged_on} onClick={() => void logout(s.target_comp_id)}>
                      Logout
                    </button>
                    <button disabled title="Session fault simulation comes in phase 6">
                      Fault…
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
