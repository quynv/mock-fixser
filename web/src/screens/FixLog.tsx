import { useEffect, useMemo, useRef, useState } from "react";
import { TagTable } from "../components/TagTable";
import { Panel } from "../components/ui";
import { messageName, parseFix, parseMirror, type FixMessage } from "../fix/parser";
import { stream } from "../fix/stream";

type Entry = { id: number; time: string; session: string; dir: string; msgType: string; raw: string; msg: FixMessage };

const MAX = 5000;

/** The U1 mirror stream (section 11.2). U1 delivery is on while this screen is open. */
export function FixLog() {
  const [entries, setEntries] = useState<Entry[]>([]);
  const [paused, setPaused] = useState(false);
  const [session, setSession] = useState("");
  const [dir, setDir] = useState("");
  const [types, setTypes] = useState("");
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<Entry>();
  const buffer = useRef<Entry[]>([]);
  const pausedRef = useRef(false);
  pausedRef.current = paused;
  const seq = useRef(0);

  useEffect(() => {
    let frame = 0;
    const flush = () => {
      frame = 0;
      if (pausedRef.current || buffer.current.length === 0) return;
      const add = buffer.current;
      buffer.current = [];
      setEntries((prev) => [...prev, ...add].slice(-MAX));
    };
    const off = stream.onLog((u1) => {
      const { session, dir, time, raw } = parseMirror(u1.raw);
      const msg = parseFix(raw);
      buffer.current.push({ id: ++seq.current, time, session, dir, msgType: msg.msgType, raw, msg });
      if (buffer.current.length > MAX) buffer.current.splice(0, buffer.current.length - MAX);
      if (!frame) frame = requestAnimationFrame(flush);
    });
    const id = setInterval(flush, 500); // picks up the buffer after a pause ends
    return () => {
      off();
      clearInterval(id);
      cancelAnimationFrame(frame);
    };
  }, []);

  const sessions = useMemo(() => [...new Set(entries.map((e) => e.session))].sort(), [entries]);
  const typeSet = new Set(types.split(",").map((t) => t.trim()).filter(Boolean));
  const shown = entries.filter(
    (e) =>
      (!session || e.session === session) &&
      (!dir || e.dir === dir) &&
      (typeSet.size === 0 || typeSet.has(e.msgType)) &&
      (!search || e.raw.includes(search)),
  );

  return (
    <div className="grid cols-2" style={{ gridTemplateColumns: "minmax(0, 3fr) minmax(0, 2fr)" }}>
      <Panel
        title={`FIX log (${shown.length})`}
        actions={
          <>
            <select value={session} onChange={(e) => setSession(e.target.value)}>
              <option value="">all sessions</option>
              {sessions.map((s) => (
                <option key={s}>{s}</option>
              ))}
            </select>
            <select value={dir} onChange={(e) => setDir(e.target.value)}>
              <option value="">in + out</option>
              <option value="I">in</option>
              <option value="O">out</option>
            </select>
            <input placeholder="MsgTypes, e.g. D,8" value={types} onChange={(e) => setTypes(e.target.value)} size={12} />
            <input placeholder="search" value={search} onChange={(e) => setSearch(e.target.value)} size={10} />
            <button onClick={() => setPaused(!paused)}>{paused ? "Resume" : "Pause"}</button>
            <button onClick={() => setEntries([])}>Clear</button>
          </>
        }
      >
        <table className="log">
          <tbody>
            {shown
              .slice(-1000)
              .reverse()
              .map((e) => (
                <tr key={e.id} className={`clickable${selected?.id === e.id ? " selected" : ""}`} onClick={() => setSelected(e)}>
                  <td>{e.time.slice(9)}</td>
                  <td className={`dir-${e.dir}`}>{e.dir === "I" ? "→ in" : "← out"}</td>
                  <td>{e.session.replace(/^FIX\.4\.4:/, "")}</td>
                  <td>{messageName(e.msgType)}</td>
                  <td className="raw">{e.raw}</td>
                </tr>
              ))}
          </tbody>
        </table>
        {entries.length === 0 && <p className="muted">Waiting for messages. Mirroring runs while this screen is open.</p>}
      </Panel>
      <Panel
        title={selected ? `${messageName(selected.msgType)} (${selected.msgType})` : "Message"}
        actions={selected && <button onClick={() => void navigator.clipboard.writeText(selected.raw)}>Copy raw</button>}
      >
        {selected ? <TagTable msg={selected.msg} /> : <p className="muted">Select a message to see its fields.</p>}
      </Panel>
    </div>
  );
}
