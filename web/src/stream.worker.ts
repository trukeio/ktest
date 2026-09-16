// The stream worker: bytes in, typed arrays out (§10.3).
//
// It reads the rack's Logb stream and does nothing else, so decoding never
// competes with drawing. For the channels the page has asked for it keeps
// every sample; for all the others only the latest value, which is what a
// readout or the channel table shows. Every 50 ms it hands the page what has
// arrived, in Float64Arrays whose buffers are transferred rather than copied.
// It hands on the panel too, whenever the stream carries one: the recording's
// panel.json is the rack's layout, and a save from any page arrives here.
//
// A dropped connection is simply reopened. A Logb stream resynchronises itself
// (§6.1): a new connection begins with a header, the current segment's schemas
// and its held values, so the rack picks up where the recording is.

import { AxisKind, AxisMode, DataType, Reader, type Batch, type Hold, type Schema, type Value } from "./logb/reader.ts";
import type { Acquisition, ChannelInfo, FromWorker, InstrumentInfo, Latest, Status, ToWorker } from "./messages.ts";

const port = self as unknown as {
  postMessage(m: FromWorker, transfer?: Transferable[]): void;
  onmessage: ((e: MessageEvent<ToWorker>) => void) | null;
};

let subs = new Set<string>();
const channels = new Map<string, ChannelInfo>();
const instruments = new Map<string, InstrumentInfo>();
let channelsChanged = false;
const pending = new Map<string, { t: number[]; v: number[] }>();
const sampled = new Set<string>(); // channels that have had a sample, not only a restatement
let latest: Record<string, Latest> = {};
let latestChanged = false;
let panel: Uint8Array | undefined; // the last panel.json the stream carried, not yet handed on
const status: Status = { state: "connecting", detail: "", bytes: 0, refused: 0 };

// The acquisitions decoded since the last flush, the newest per stream. An
// acquisition replaces its predecessor: a waveform is a picture taken again,
// not a series that grows, and drawing a stale one is worse than drawing none.
const acqs = new Map<string, Acquisition>();

function numeric(v: Value): number | undefined {
  if (typeof v === "number") return v;
  if (typeof v === "bigint") return Number(v);
  if (typeof v === "boolean") return v ? 1 : 0;
  return undefined;
}

function display(v: Value): Latest {
  if (typeof v === "bigint") return Number(v);
  if (v instanceof Uint8Array) return [...v].map((x) => x.toString(16).padStart(2, "0")).join("");
  if (Array.isArray(v)) return `${v[0]} ${v[1] < 0 ? "-" : "+"} ${Math.abs(v[1])}i`;
  return v;
}

/** envelopeOf is the channel a stream is the envelope of, or "" if it is not one. */
function envelopeOf(s: Schema): string {
  return s.meta["kind"] === "envelope" ? (s.meta["of"] ?? "") : "";
}

function note(s: Schema) {
  // Each instrument says what kind it is in its control stream's schema, which
  // is what decides the path its lease is taken on (§7). Reading it from the
  // recording rather than guessing from the shape of a name is what keeps a
  // third kind of instrument from silently becoming a supply.
  if (s.name.endsWith(".control") && s.meta["instrument"]) {
    const inst = { name: s.meta["instrument"], kind: s.meta["kind"] ?? "instrument" };
    if (instruments.get(inst.name)?.kind !== inst.kind) {
      instruments.set(inst.name, inst);
      channelsChanged = true;
    }
  }
  // An envelope is not a channel anyone binds a readout or a plot to (§4): it
  // is one acquisition's reduction, replaced whole at every trigger, and
  // listing its min and max beside the rack's real channels would say the
  // recording has a signal called "scope1.ch1.env.min" that holds a value.
  //
  // The channel it reduces is listed instead, as a waveform, so that a panel
  // can be bound to a scope channel before its first trigger rather than only
  // after one.
  const of = envelopeOf(s);
  if (of) {
    if (!channels.has(of)) {
      channels.set(of, { name: of, stream: of, field: "", unit: "V", kind: "waveform", held: false });
      channelsChanged = true;
    }
    return;
  }
  for (const f of s.fields) {
    if (f.type === DataType.Bytes || f.type === DataType.Complex) continue;
    const name = `${s.name}.${f.name}`;
    if (channels.has(name)) continue;
    const text = f.type === DataType.String || f.conv.kind === "v2t" || f.conv.kind === "r2t";
    channels.set(name, { name, stream: s.name, field: f.name, unit: f.unit, kind: text ? "text" : "number", held: f.held });
    channelsChanged = true;
  }
}

function sample(name: string, t: number, x: number) {
  let p = pending.get(name);
  if (!p) pending.set(name, (p = { t: [], v: [] }));
  p.t.push(t);
  p.v.push(x);
  sampled.add(name);
}

// onAcquisition turns one envelope batch into the arrays a widget draws from.
//
// It is a separate path from onData for the reason §10.3 gives: the live path
// has no gaps to express and nothing is boxed on the way. A bucket that held no
// sample is NaN, which uPlot breaks the line at, and its n is 0 beside it.
function onAcquisition(b: Batch, of: string) {
  const s = b.schema;
  const params = b.run?.params ?? {};
  const iMin = s.fields.findIndex((f) => f.name === "min");
  const iMax = s.fields.findIndex((f) => f.name === "max");
  const iN = s.fields.findIndex((f) => f.name === "n");
  if (iMin < 0 || iMax < 0 || iN < 0 || s.axisMode !== AxisMode.Implicit) return;

  const n = b.count;
  const t = new Float64Array(n);
  const lo = new Float64Array(n);
  const hi = new Float64Array(n);
  const counts = new Int32Array(n);
  // The acquisition's own time is where the trigger is, as the daemon placed
  // it; the buckets are drawn against it so that 0 is the trigger whatever the
  // recording's clock says.
  const at = b.seconds(0) - Number(params["first"] ? seconds(params["first"]) : 0);
  for (let i = 0; i < n; i++) {
    t[i] = b.seconds(i) - at;
    const c = b.value(i, iN);
    counts[i] = typeof c === "number" ? c : Number(c ?? 0);
    const a = b.value(i, iMin);
    const z = b.value(i, iMax);
    // Absent, not zero: the bucket held no sample and the chart must break.
    lo[i] = a === undefined ? NaN : Number(a);
    hi[i] = z === undefined ? NaN : Number(z);
  }
  acqs.set(s.name, { stream: s.name, of, run: b.runId, t, min: lo, max: hi, n: counts, at, params });
}

/** seconds reads a Go duration as the RUN frame writes one: "-7ms", "1µs". */
function seconds(d: string): number {
  const m = /^([+-]?[0-9.]+)(ns|us|µs|ms|s|m|h)$/.exec(d.trim());
  if (!m) return Number(d) || 0;
  const unit: Record<string, number> = { ns: 1e-9, us: 1e-6, "µs": 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  return Number(m[1]) * unit[m[2]];
}

function onData(b: Batch) {
  const s = b.schema;
  const of = envelopeOf(s);
  if (of) {
    onAcquisition(b, of);
    return;
  }
  for (let f = 0; f < s.fields.length; f++) {
    const fd = s.fields[f];
    if (fd.type === DataType.Bytes || fd.type === DataType.Complex) continue;
    const name = `${s.name}.${fd.name}`;
    if (subs.has(name)) {
      for (let i = 0; i < b.count; i++) {
        const v = b.value(i, f);
        const x = v === undefined ? undefined : numeric(v);
        if (x !== undefined) sample(name, b.seconds(i), x);
      }
    }
    // The latest value is the last record that carries the field: a guarded
    // field may be absent from the ones after it.
    for (let i = b.count - 1; i >= 0; i--) {
      const v = b.value(i, f);
      if (v !== undefined) {
        latest[name] = display(v);
        latestChanged = true;
        break;
      }
    }
  }
}

// A restatement gives a held channel its value in force at the segment's start
// (§5.1): what a readout shows at once on joining, and the start of a plot's
// step trace — but only before the channel's first real sample, since it is a
// statement about the past, never a sample (SPEC §6.10).
function onHold(h: Hold) {
  const s = h.schema;
  const at = s.axisKind === AxisKind.Time ? Number(h.axisBase) * 10 ** s.axisExp : (h.axisBase as number);
  s.fields.forEach((fd, f) => {
    const v = h.value(f);
    if (v === undefined) return;
    const name = `${s.name}.${fd.name}`;
    latest[name] = display(v);
    latestChanged = true;
    const x = numeric(v);
    if (x !== undefined && subs.has(name) && !sampled.has(name)) sample(name, at, x);
  });
}

function flush() {
  // An acquisition replaces its predecessor, so only the newest of each stream
  // is handed on: a page that fell behind draws what is on screen now rather
  // than catching up through pictures nobody will see.
  for (const [name, a] of acqs) {
    acqs.delete(name);
    port.postMessage({ type: "acq", acq: a }, [a.t.buffer, a.min.buffer, a.max.buffer, a.n.buffer]);
  }
  // A late joiner's preamble carries every panel ever saved, in order; only
  // the last is in force, and only it is handed on.
  if (panel !== undefined) {
    const data = new Uint8Array(panel);
    panel = undefined;
    void crypto.subtle.digest("SHA-256", data).then((h) => {
      const sha = [...new Uint8Array(h)].map((x) => x.toString(16).padStart(2, "0")).join("");
      port.postMessage({ type: "panel", text: new TextDecoder().decode(data), sha });
    });
  }
  if (channelsChanged) {
    channelsChanged = false;
    port.postMessage({ type: "channels", channels: [...channels.values()], instruments: [...instruments.values()] });
  }
  if (pending.size > 0 || latestChanged) {
    const series: Record<string, { t: Float64Array; v: Float64Array }> = {};
    const transfer: Transferable[] = [];
    for (const [name, p] of pending) {
      const t = Float64Array.from(p.t);
      const v = Float64Array.from(p.v);
      series[name] = { t, v };
      transfer.push(t.buffer, v.buffer);
    }
    pending.clear();
    port.postMessage({ type: "data", series, latest }, transfer);
    latest = {};
    latestChanged = false;
  }
  port.postMessage({ type: "status", status });
}

async function run(url: string, token: string) {
  for (;;) {
    status.state = "connecting";
    status.detail = "";
    try {
      const res = await fetch(url, { headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
      if (res.status === 401) {
        status.state = "error";
        status.detail = "the token was refused";
        return;
      }
      if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
      port.postMessage({ type: "reset" });
      sampled.clear();
      const reader = new Reader({
        schema: note,
        data: onData,
        hold: onHold,
        attach(name, data) {
          if (name === "panel.json") panel = data;
        },
        refused(r) {
          status.refused++;
          status.detail = `${r.stream}: ${r.why}`;
        },
      });
      status.state = "live";
      const body = res.body.getReader();
      for (;;) {
        const { done, value } = await body.read();
        if (done) break;
        status.bytes += value.length;
        reader.push(value);
        if (reader.broken) throw new Error(`the stream is damaged: ${reader.broken}`);
      }
      status.state = "ended";
      status.detail = "the recording ended";
    } catch (e) {
      status.state = "error";
      status.detail = e instanceof Error ? e.message : String(e);
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
}

port.onmessage = (e) => {
  const m = e.data;
  switch (m.type) {
    case "start":
      setInterval(flush, 50);
      void run(m.url, m.token);
      break;
    case "subscribe":
      subs = new Set(m.channels);
      break;
  }
};
