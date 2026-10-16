import { enumName, fieldName, type FixMessage } from "../fix/parser";

/** Tag-by-tag view of a FIX message with field names and enum values (section 11.2). */
export function TagTable({ msg }: { msg: FixMessage }) {
  return (
    <table className="tags mono">
      <tbody>
        {msg.fields.map((f, i) => {
          const e = enumName(f.tag, f.value);
          return (
            <tr key={i}>
              <td className="num">{f.tag}</td>
              <td>{fieldName(f.tag)}</td>
              <td>
                {f.value}
                {e && <span className="muted"> ({e})</span>}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
