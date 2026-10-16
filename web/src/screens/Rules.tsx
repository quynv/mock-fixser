import { useState } from "react";
import { api, HttpError, type RuleTestResult } from "../api";
import { Field, Input, Panel, Select, toNum } from "../components/ui";
import { go } from "../router";
import { draftJS, editDraft, errorsAt, itemIndex, setAt, useConfig } from "../state/config";
import { condLabel, FIELDS, fromMatch, opsFor, ORD_TYPES, TIFS, timeline, toMatch, type Action, type Cond } from "../state/rules";

type Rule = { name: string; priority: number; message_type?: string; match?: Record<string, unknown>; actions?: Action[] };
type ListRule = { name: string; priority: number; match?: Record<string, unknown>; action?: Action };

const ACTION_TYPES: Record<string, string[]> = {
  D: ["ack", "fill", "partial_fill", "reject", "cancel", "expire", "ignore", "business_reject"],
  F: ["ack", "reject", "ignore"],
  G: ["ack", "reject", "ignore"],
};

export function Rules({ selected }: { selected?: string }) {
  const cfg = useConfig();
  const data = draftJS(cfg.draft);
  const rules = [...((data.order_rules as Rule[] | undefined) ?? [])].filter(Boolean).sort((a, b) => a.priority - b.priority);
  const [drag, setDrag] = useState<string>();
  const [over, setOver] = useState<string>();
  const current = rules.find((r) => r.name === selected) ?? rules[0];

  // Drag and drop sets priorities 10, 20, 30... in the new order.
  const drop = (target: string) => {
    setOver(undefined);
    if (!drag || drag === target) return;
    const names = rules.map((r) => r.name).filter((n) => n !== drag);
    names.splice(names.indexOf(target), 0, drag);
    editDraft((doc) => {
      names.forEach((n, i) => doc.setIn(["order_rules", itemIndex(data, "order_rules", "name", n), "priority"], (i + 1) * 10));
    });
  };

  const add = () => {
    let name = "new-rule";
    for (let i = 2; rules.some((r) => r.name === name); i++) name = `new-rule-${i}`;
    const top = rules.length ? Math.max(...rules.map((r) => r.priority)) + 10 : 10;
    editDraft((doc) => doc.addIn(["order_rules"], { name, priority: top, match: {}, actions: [{ type: "ack" }, { type: "fill" }] }));
    go("rules", name);
  };

  return (
    <div className="grid">
      <div className="split" style={{ gridTemplateColumns: "320px 1fr" }}>
        <Panel title="Order rules" actions={<button onClick={add}>Add</button>}>
          <p className="muted">First match by priority wins. Drag to reorder.</p>
          <ul className="list">
            {rules.map((r) => (
              <li
                key={r.name}
                draggable
                onDragStart={() => setDrag(r.name)}
                onDragOver={(e) => {
                  e.preventDefault();
                  setOver(r.name);
                }}
                onDragLeave={() => setOver(undefined)}
                onDrop={() => drop(r.name)}
                className={`${r.name === current?.name ? "selected" : ""}${over === r.name ? " dragover" : ""}`}
                onClick={() => go("rules", r.name)}
                style={{ flexDirection: "column", alignItems: "stretch" }}
              >
                <div className="row" style={{ justifyContent: "space-between" }}>
                  <span>
                    ⠿ {r.name}
                    {r.message_type && r.message_type !== "D" ? <span className="chip"> {r.message_type}</span> : null}
                  </span>
                  <span className="muted">{r.priority}</span>
                </div>
                <div className="chips">
                  {fromMatch(r.match).map((c, i) => (
                    <span className="chip" key={i}>
                      {condLabel(c)}
                    </span>
                  ))}
                </div>
                <Timeline actions={r.actions} />
              </li>
            ))}
          </ul>
        </Panel>
        {current ? (
          <RuleEditor key={current.name} rule={current} index={itemIndex(data, "order_rules", "name", current.name)} />
        ) : (
          <p className="muted">No rules: every order gets default_action.</p>
        )}
      </div>
      <div className="grid cols-2">
        <Panel title="default_action (no rule matched)">
          <ActionsEditor path={["default_action"]} errPath="default_action" msgType="D" actions={data.default_action as Action[] | undefined} />
          <p className="muted">Empty means ack, then fill.</p>
        </Panel>
        <Tester draft={cfg.draft} />
      </div>
      <ListRules rules={(data.list_rules as ListRule[] | undefined) ?? []} />
    </div>
  );
}

function Timeline({ actions }: { actions?: Action[] }) {
  const t = timeline(actions);
  return (
    <div className="timeline">
      {t.steps.map((s, i) => (
        <span key={i} className="row" style={{ gap: 0 }}>
          {i > 0 && <span className="arrow">→</span>}
          <span className="step">
            {s.label}
            {s.at ? <span className="muted"> {s.at}ms</span> : null}
          </span>
        </span>
      ))}
      {t.total > 0 && <span className="muted" style={{ marginLeft: 8 }}>total {t.total} ms</span>}
    </div>
  );
}

function RuleEditor({ rule, index }: { rule: Rule; index: number }) {
  const p = `order_rules[${index}]`;
  const at = (...k: (string | number)[]) => ["order_rules", index, ...k];
  const msgType = rule.message_type ?? "D";
  const { errors } = useConfig();

  const remove = () => {
    if (!confirm(`Delete rule ${rule.name} from the draft?`)) return;
    editDraft((doc) => doc.deleteIn(["order_rules", index]));
    go("rules");
  };

  return (
    <Panel title={`Rule ${rule.name}`} actions={<button className="danger" onClick={remove}>Delete</button>}>
      <div className="form">
        <Field label="Name" path={`${p}.name`}>
          <Input
            value={rule.name}
            onCommit={(v) => {
              setAt(at("name"), v);
              go("rules", v);
            }}
          />
        </Field>
        <Field label="Priority" path={`${p}.priority`}>
          <Input type="number" value={rule.priority} onCommit={(v) => setAt(at("priority"), toNum(v))} />
        </Field>
        <Field label="Message type" path={`${p}.message_type`}>
          <Select
            value={msgType}
            options={["D", "F", "G"]}
            onChange={(v) => {
              editDraft((doc) => {
                if (v === "D") doc.deleteIn(at("message_type"));
                else {
                  doc.setIn(at("message_type"), v);
                  doc.setIn(at("actions"), [{ type: "ack" }]);
                }
              });
            }}
          />
        </Field>
      </div>
      <h4>Conditions (all must match)</h4>
      <ConditionsEditor path={at("match")} errPath={`${p}.match`} match={rule.match} />
      <h4>Actions</h4>
      {msgType !== "D" && (
        <p className="muted">
          {msgType === "F" ? "Cancel" : "Replace"} rules take one action: ack (accept, optionally after a delay), reject or ignore.
        </p>
      )}
      <ActionsEditor path={at("actions")} errPath={`${p}.actions`} msgType={msgType} actions={rule.actions} />
      {errorsAt(errors, p)
        .filter((e) => !e.path.includes(".match") && !e.path.includes(".actions"))
        .map((e, i) => (
          <p key={i} className="err-text">
            {e.path}: {e.message}
          </p>
        ))}
    </Panel>
  );
}

function ConditionsEditor({ path, errPath, match }: { path: (string | number)[]; errPath: string; match?: Record<string, unknown> }) {
  const rows = fromMatch(match);
  const { errors } = useConfig();
  const write = (next: Cond[]) => {
    const m = toMatch(next);
    setAt(path, Object.keys(m).length ? m : undefined);
  };
  const update = (i: number, patch: Partial<Cond>) => {
    const next = rows.map((r, j) => (j === i ? { ...r, ...patch } : r));
    if (patch.field) next[i].op = opsFor(patch.field)[0];
    write(next);
  };
  return (
    <div className="grid" style={{ gap: 6 }}>
      {rows.map((r, i) => (
        <div className="row" key={i}>
          <Select value={r.field} options={FIELDS} onChange={(v) => update(i, { field: v, value: v === "in_list" ? "true" : r.value })} />
          {r.field === "tags" && <Input value={r.tag} placeholder="tag" onCommit={(v) => update(i, { tag: v })} />}
          <Select value={r.op} options={opsFor(r.field)} onChange={(v) => update(i, { op: v })} />
          {r.field === "side" && r.op === "eq" ? (
            <Select value={r.value} options={["BUY", "SELL"]} onChange={(v) => update(i, { value: v })} />
          ) : (r.field === "ord_type" || r.field === "time_in_force") && r.op === "eq" ? (
            <Select value={r.value} options={r.field === "ord_type" ? ORD_TYPES : TIFS} onChange={(v) => update(i, { value: v })} />
          ) : r.field === "in_list" ? (
            <Select value={r.value} options={["true", "false"]} onChange={(v) => update(i, { value: v })} />
          ) : (
            <Input value={r.value} placeholder={r.op === "in" ? "a, b, c" : "value"} onCommit={(v) => update(i, { value: v })} />
          )}
          <button className="link" onClick={() => write(rows.filter((_, j) => j !== i))}>
            remove
          </button>
        </div>
      ))}
      <div>
        <button onClick={() => write([...rows, { field: "symbol", op: "eq", value: "" }])}>Add condition</button>
      </div>
      {errorsAt(errors, errPath).map((e, i) => (
        <p key={i} className="err-text">
          {e.path}: {e.message}
        </p>
      ))}
    </div>
  );
}

function ActionsEditor({
  path,
  errPath,
  msgType,
  actions = [],
}: {
  path: (string | number)[];
  errPath: string;
  msgType: string;
  actions?: Action[];
}) {
  const { errors } = useConfig();
  const write = (next: Action[]) => setAt(path, next.length ? next : undefined);
  const update = (i: number, key: string, v: unknown) =>
    write(
      actions.map((a, j) => {
        if (j !== i) return a;
        const n: Action = { ...a, [key]: v };
        if (v === undefined || v === "") delete n[key];
        return n;
      }),
    );
  const move = (i: number, d: number) => {
    const next = [...actions];
    [next[i], next[i + d]] = [next[i + d], next[i]];
    write(next);
  };
  return (
    <div className="grid" style={{ gap: 6 }}>
      {actions.map((a, i) => (
        <div className="row" key={i}>
          <span className="muted">{i + 1}.</span>
          <Select value={a.type} options={ACTION_TYPES[msgType]} onChange={(v) => write(actions.map((x, j) => (j === i ? { type: v } : x)))} />
          <label className="muted">delay</label>
          <Input type="number" value={a.delay_ms} placeholder="0" onCommit={(v) => update(i, "delay_ms", toNum(v))} />
          {a.type === "partial_fill" && (
            <>
              <label className="muted">ratio</label>
              <Input type="number" value={a.ratio} onCommit={(v) => update(i, "ratio", toNum(v))} />
              <label className="muted">or qty</label>
              <Input type="number" value={a.qty} onCommit={(v) => update(i, "qty", toNum(v))} />
              <label className="muted">count</label>
              <Input type="number" value={a.count} placeholder="1" onCommit={(v) => update(i, "count", toNum(v))} />
            </>
          )}
          {(a.type === "fill" || a.type === "partial_fill") && (
            <>
              <Select
                value={a.price_mode ?? "market"}
                options={["market", "market_driven", "limit_immediate", "fixed"]}
                onChange={(v) => update(i, "price_mode", v === "market" ? undefined : v)}
              />
              {a.price_mode === "fixed" && <Input type="number" value={a.price} placeholder="price" onCommit={(v) => update(i, "price", toNum(v))} />}
            </>
          )}
          {(a.type === "reject" || a.type === "business_reject") && (
            <>
              <label className="muted">reason</label>
              <Input type="number" value={a.reason} onCommit={(v) => update(i, "reason", toNum(v))} />
            </>
          )}
          {["reject", "business_reject", "cancel", "expire"].includes(a.type) && (
            <Input value={a.text} placeholder="text" onCommit={(v) => update(i, "text", v)} />
          )}
          <button className="link" disabled={i === 0} onClick={() => move(i, -1)}>
            ↑
          </button>
          <button className="link" disabled={i === actions.length - 1} onClick={() => move(i, 1)}>
            ↓
          </button>
          <button className="link" onClick={() => write(actions.filter((_, j) => j !== i))}>
            remove
          </button>
        </div>
      ))}
      <div className="row">
        <button disabled={msgType !== "D" && actions.length >= 1} onClick={() => write([...actions, { type: msgType === "D" ? "fill" : "ack" }])}>
          Add action
        </button>
        <Timeline actions={actions} />
      </div>
      {errorsAt(errors, errPath).map((e, i) => (
        <p key={i} className="err-text">
          {e.path}: {e.message}
        </p>
      ))}
    </div>
  );
}

/** Rule tester: a sample order against the draft (section 11.2). */
function Tester({ draft }: { draft: string }) {
  const [o, setO] = useState<Record<string, string>>({ message_type: "D", symbol: "", side: "BUY", ord_type: "MARKET", time_in_force: "DAY", qty: "100", cl_ord_id: "", comp_id: "", tags: "" });
  const [res, setRes] = useState<RuleTestResult>();
  const [err, setErr] = useState<string>();
  const set = (k: string) => (v: string) => setO({ ...o, [k]: v });
  const run = async () => {
    setErr(undefined);
    try {
      const tags = Object.fromEntries(
        o.tags.split(",").map((kv) => kv.split("=").map((s) => s.trim())).filter((kv) => kv[0] && kv[1] !== undefined),
      );
      setRes(
        await api.testRule({
          message_type: o.message_type, symbol: o.symbol, side: o.side, ord_type: o.ord_type, time_in_force: o.time_in_force,
          qty: o.qty || "0", price: o.price || undefined,
          account: o.account, cl_ord_id: o.cl_ord_id, comp_id: o.comp_id, tags,
          in_list: o.list_id ? true : undefined, list_id: o.list_id, list_seq_no: o.list_seq_no ? Number(o.list_seq_no) : undefined,
          yaml: draft,
        }),
      );
    } catch (e) {
      setRes(undefined);
      setErr(e instanceof HttpError ? `${e.body.message}${e.body.details.length ? `: ${e.body.details[0].path} ${e.body.details[0].message}` : ""}` : String(e));
    }
  };
  return (
    <Panel title="Rule tester (uses the draft)" actions={<button className="primary" onClick={() => void run()}>Test</button>}>
      <div className="form">
        <label className="field">
          <span className="muted">Message</span>
          <Select value={o.message_type} options={["D", "F", "G"]} onChange={set("message_type")} />
        </label>
        {(["symbol", "qty", "price", "account", "cl_ord_id", "comp_id", "list_id", "list_seq_no"] as const).map((k) => (
          <label className="field" key={k}>
            <span className="muted">{k}</span>
            <input value={o[k] ?? ""} onChange={(e) => set(k)(e.target.value)} />
          </label>
        ))}
        <label className="field">
          <span className="muted">side</span>
          <Select value={o.side} options={["BUY", "SELL"]} onChange={set("side")} />
        </label>
        <label className="field">
          <span className="muted">ord_type</span>
          <Select value={o.ord_type} options={ORD_TYPES} onChange={set("ord_type")} />
        </label>
        <label className="field">
          <span className="muted">time_in_force</span>
          <Select value={o.time_in_force} options={TIFS} onChange={set("time_in_force")} />
        </label>
        <label className="field">
          <span className="muted">tags (9001=VIP, …)</span>
          <input value={o.tags} onChange={(e) => set("tags")(e.target.value)} />
        </label>
      </div>
      {err && <p className="err-text">{err}</p>}
      {res && (
        <div style={{ marginTop: 10 }}>
          <p>
            Matches <strong className="mono">{res.rule || "(none: accepted at once)"}</strong>
          </p>
          <div className="timeline">
            {res.actions.map((a, i) => (
              <span key={i} className="row" style={{ gap: 0 }}>
                {i > 0 && <span className="arrow">→</span>}
                <span className="step">
                  {a.type}
                  {a.count && a.count > 1 ? ` ×${a.count}` : ""} <span className="muted">@{a.at_ms}ms</span>
                </span>
              </span>
            ))}
            <span className="muted" style={{ marginLeft: 8 }}>
              total {res.total_ms} ms
            </span>
          </div>
        </div>
      )}
    </Panel>
  );
}

/** list_rules for NewOrderList (section 7.6). */
function ListRules({ rules }: { rules: ListRule[] }) {
  const add = () => {
    let name = "new-list-rule";
    for (let i = 2; rules.some((r) => r.name === name); i++) name = `new-list-rule-${i}`;
    editDraft((doc) => doc.addIn(["list_rules"], { name, priority: (rules.length + 1) * 10, match: { tot_no_orders: { gt: 50 } }, action: { type: "reject_list", text: "List too large" } }));
  };
  const at = (i: number, ...k: string[]) => ["list_rules", i, ...k];
  return (
    <Panel title="List rules (NewOrderList)" actions={<button onClick={add}>Add</button>}>
      {rules.length === 0 && <p className="muted">No list rules.</p>}
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Priority</th>
            <th>tot_no_orders &gt;</th>
            <th>list_id regex</th>
            <th>Action</th>
            <th>Text / delay (ms)</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {rules.map((r, i) => {
            const tot = (r.match?.tot_no_orders as { gt?: number } | undefined)?.gt;
            const re = (r.match?.list_id as { regex?: string } | undefined)?.regex;
            return (
              <tr key={i}>
                <td><Input value={r.name} onCommit={(v) => setAt(at(i, "name"), v)} /></td>
                <td><Input type="number" value={r.priority} onCommit={(v) => setAt(at(i, "priority"), toNum(v))} /></td>
                <td><Input type="number" value={tot} onCommit={(v) => setAt(at(i, "match", "tot_no_orders"), v ? { gt: Number(v) } : undefined)} /></td>
                <td><Input value={re} onCommit={(v) => setAt(at(i, "match", "list_id"), v ? { regex: v } : undefined)} /></td>
                <td>
                  <Select
                    value={r.action?.type}
                    options={["reject_list", "delay_ack"]}
                    onChange={(v) => setAt(at(i, "action"), v === "reject_list" ? { type: v, text: "List rejected" } : { type: v, delay_ms: 1000 })}
                  />
                </td>
                <td>
                  {r.action?.type === "delay_ack" ? (
                    <Input type="number" value={r.action.delay_ms} onCommit={(v) => setAt(at(i, "action", "delay_ms"), toNum(v))} />
                  ) : (
                    <Input value={r.action?.text} onCommit={(v) => setAt(at(i, "action", "text"), v)} />
                  )}
                </td>
                <td>
                  <button className="link" onClick={() => editDraft((doc) => doc.deleteIn(["list_rules", i]))}>
                    remove
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </Panel>
  );
}
