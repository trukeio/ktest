// acq runs the rack worker's envelope path over a file and prints, for each
// acquisition, the axis a scope widget would draw it against.
//
//	node test/acq.ts FILE
//
// It exists because that axis is computed in one place and checked nowhere
// else: the daemon places an acquisition at a userspace stamp taken at readout
// and writes the envelope relative to that, and the widget subtracts it again
// so that the trigger is at zero. Getting it wrong draws a trace half a screen
// out, with nothing to say so — and a scope trace drawn in the wrong place is
// the kind of wrong answer that looks entirely plausible.

import { readFileSync } from "node:fs";
import { AxisMode, Reader } from "../src/logb/reader.ts";

/** seconds reads a Go duration as a RUN frame writes one: "-7ms", "1µs". */
function seconds(d: string): number {
  const m = /^([+-]?[0-9.]+)(ns|us|µs|ms|s|m|h)$/.exec(d.trim());
  if (!m) return Number(d) || 0;
  const unit: Record<string, number> = { ns: 1e-9, us: 1e-6, "µs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  return Number(m[1]) * unit[m[2]];
}

const r = new Reader({
  data(b) {
    const s = b.schema;
    if (s.meta["kind"] !== "envelope" || s.axisMode !== AxisMode.Implicit) return;
    const p = b.run?.params ?? {};
    const at = b.seconds(0) - (p["first"] ? seconds(p["first"]) : 0);
    const iMin = s.fields.findIndex((f) => f.name === "min");
    const iMax = s.fields.findIndex((f) => f.name === "max");
    const iN = s.fields.findIndex((f) => f.name === "n");

    const t = new Float64Array(b.count);
    let empty = 0;
    let lo = Infinity;
    let hi = -Infinity;
    for (let i = 0; i < b.count; i++) {
      t[i] = b.seconds(i) - at;
      const mn = b.value(i, iMin);
      const mx = b.value(i, iMax);
      const n = Number(b.value(i, iN) ?? 0);
      // A bucket with no sample must come back absent, never as a zero volt.
      if (mn === undefined || mx === undefined) {
        if (n !== 0) throw new Error(`${s.name}[${i}]: absent with n=${n}`);
        empty++;
        continue;
      }
      if (n === 0) throw new Error(`${s.name}[${i}]: present with n=0`);
      lo = Math.min(lo, Number(mn));
      hi = Math.max(hi, Number(mx));
    }
    const span = t[t.length - 1] - t[0];
    console.log(JSON.stringify({
      s: s.name,
      run: b.runId,
      buckets: b.count,
      empty,
      // Rounded, because these are a comparison's subject and not a
      // reproduction of the bytes: the Go side computes them the same way.
      first: Number(t[0].toPrecision(12)),
      last: Number(t[t.length - 1].toPrecision(12)),
      span: Number(span.toPrecision(12)),
      // Where the trigger falls across the trace. Half the screen is before it.
      triggerFrac: Number(((0 - t[0]) / span).toPrecision(6)),
      min: Number(lo.toPrecision(9)),
      max: Number(hi.toPrecision(9)),
      tdiv: p["tdiv"] ?? "",
      vdiv: p["vdiv"] ?? "",
    }));
  },
});
r.push(new Uint8Array(readFileSync(process.argv[2])));
if (r.broken) {
  console.error("broken:", r.broken);
  process.exit(1);
}
