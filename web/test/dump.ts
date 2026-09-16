// dump prints everything the rack's Logb reader decodes from a file, in the
// canonical form web/reader_test.go prints the Go reader's with, so that the two
// can be compared line by line.
//
//	node test/dump.ts FILE [CHUNK]
//
// CHUNK feeds the file in pieces: 0 (the default) whole, a number in pieces of
// that many bytes, "random" in pieces of 1 to 4096 bytes from a fixed seed. A
// live stream arrives in pieces nobody chose, so the reader must not care.

import { readFileSync } from "node:fs";
import { AxisKind, DataType, Reader, type Schema, type Value } from "../src/logb/reader.ts";

const [path, chunkArg = "0"] = process.argv.slice(2);
const data = new Uint8Array(readFileSync(path));

function hex(b: Uint8Array) {
  let s = "";
  for (const x of b) s += x.toString(16).padStart(2, "0");
  return s;
}

function fnv(b: Uint8Array) {
  let h = 0x811c9dc5;
  for (const x of b) h = Math.imul(h ^ x, 0x01000193) >>> 0;
  return h.toString(16).padStart(8, "0");
}

function num(x: number): unknown {
  if (Number.isFinite(x)) return x;
  return { f: Number.isNaN(x) ? "NaN" : x > 0 ? "+Inf" : "-Inf" };
}

function enc(s: Schema, f: number, v: Value): unknown {
  const fd = s.fields[f];
  if (Array.isArray(v)) return { c: [num(v[0]), num(v[1])] };
  if (typeof v === "boolean") return v;
  if (typeof v === "string") return { s: v };
  if (v instanceof Uint8Array) return { x: hex(v) };
  if (typeof v === "bigint") return { i: v.toString() };
  // A number: an integer field read as itself, or a float.
  if (fd.conv.kind === "identity" && (fd.type === DataType.Uint || fd.type === DataType.Sint)) return { i: String(v) };
  return num(v);
}

function axis(s: Schema, a: bigint | number): unknown {
  return s.axisKind === AxisKind.Time ? { i: String(a) } : num(a as number);
}

const lines: string[] = [];
const metas: string[] = [];
const attaches: [string, Uint8Array][] = [];
let refused = 0;

const r = new Reader({
  data(b) {
    const s = b.schema;
    // The run a batch belongs to, printed once per batch: a scope acquisition
    // carries the settings its samples were taken under here (§5.4), and a
    // reader that decoded the samples and lost the run would look right.
    if (b.run) {
      lines.push(JSON.stringify({ k: "run", s: s.name, id: b.run.id, index: b.run.index, params: b.run.params }));
    }
    for (let i = 0; i < b.count; i++) {
      const v: Record<string, unknown> = {};
      s.fields.forEach((fd, f) => {
        const val = b.value(i, f);
        if (val !== undefined) v[fd.name] = enc(s, f, val);
      });
      lines.push(JSON.stringify({ k: "rec", s: s.name, i, axis: axis(s, b.axis(i)), v }));
    }
  },
  hold(h) {
    const s = h.schema;
    const v: Record<string, unknown> = {};
    s.fields.forEach((fd, f) => {
      const val = h.value(f);
      if (val !== undefined) v[fd.name] = enc(s, f, val);
    });
    lines.push(JSON.stringify({ k: "hold", s: s.name, axis: axis(s, h.axisBase), v }));
  },
  meta(key, value) {
    metas.push(JSON.stringify({ k: "meta", key, value }));
  },
  attach(name, d) {
    attaches.push([name, d]);
  },
  refused() {
    refused++;
  },
});

// mulberry32: a small seeded generator, so a "random" run is repeatable.
let seed = 1;
function rand() {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}

if (chunkArg === "0") {
  r.push(data);
} else {
  for (let o = 0; o < data.length; ) {
    const n = chunkArg === "random" ? 1 + Math.floor(rand() * 4096) : Number(chunkArg);
    r.push(data.subarray(o, o + n));
    o += n;
  }
}

for (const l of lines) console.log(l);
for (const m of metas) console.log(m);
attaches.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
for (const [name, d] of attaches) console.log(JSON.stringify({ k: "attach", name, len: d.length, fnv: fnv(d) }));
console.log(JSON.stringify({ k: "refused", n: refused }));
console.log(JSON.stringify({ k: "truncated", v: r.broken !== "" }));
if (r.broken) console.error("broken:", r.broken);
