// A Logb reader for the browser.
//
// It is fed bytes as they arrive — a live stream is an HTTP response body read
// in chunks — and decodes whole frames only, so a chunk that ends mid-frame is
// the normal case rather than an edge one. It reads what ktestd sends a
// browser: every frame type, every field type and conversion, and DATA frames
// with codec none, transposed or not. A codec it does not carry is refused
// frame by frame, as SPEC §8 requires, and never guessed at.
//
// It follows logb's Go reader, not only the specification's prose, and is held
// to it: web/reader_test.go writes recordings with the Go writer, reads them with
// both readers, and requires the same values from each, fed whole and in pieces.
//
// Values keep their type (SPEC §7): a bool reads back as a boolean, a string as
// a string. Integers are numbers when a number holds them exactly and bigints
// when it cannot, so no value is ever rounded on the way in.

const MAGIC = [0x89, 0x4c, 0x4f, 0x47, 0x42, 0x0d, 0x0a, 0x1a];
const SYNC_PATTERN = [0x4c, 0x4f, 0x47, 0x42, 0x53, 0x59, 0x4e, 0x43, 0xa7, 0x3e, 0x91, 0xd2, 0x5c, 0x68, 0x0b, 0xf4];

export const FrameType = {
  Sync: 0x01, Schema: 0x10, Meta: 0x11, Attach: 0x12, Run: 0x13, Hold: 0x14, Data: 0x20, Index: 0x30, End: 0x40,
} as const;

export const DataType = { Uint: 0, Sint: 1, Float: 2, Bool: 3, Bytes: 4, String: 5, Complex: 6 } as const;
export const AxisKind = { Time: 0, Frequency: 1, Angle: 2, Distance: 3, Index: 4, Other: 5 } as const;
export const AxisMode = { Implicit: 0, Explicit: 1, Log: 2 } as const;

export type Conversion =
  | { kind: "identity" }
  | { kind: "linear"; a: number; b: number }
  | { kind: "rational"; p: number[] }
  | { kind: "table"; keys: number[]; vals: number[]; interp: boolean }
  | { kind: "v2t"; keys: number[]; texts: string[]; def: string }
  | { kind: "r2t"; los: number[]; his: number[]; texts: string[]; def: string };

export interface Field {
  name: string;
  bitOffset: number;
  bitWidth: number;
  type: number;
  bigEndian: boolean;
  variable: boolean;
  guarded: boolean;
  held: boolean;
  unit: string;
  desc: string;
  conv: Conversion;
  guardField: number;
  guardValue: bigint;
  guardNum: number; // guardValue as a number, NaN when a number cannot hold it
  meta: Record<string, string>;
}

export interface Schema {
  streamId: number;
  uuid: string;
  name: string;
  recordBits: number;
  recordBytes: number;
  axisKind: number;
  axisMode: number;
  axisExp: number;
  axisUnit: string;
  axisStep: bigint | number; // ticks for a time axis, else a float
  axisScale: bigint | number;
  axisField: number;
  fields: Field[];
  meta: Record<string, string>;
  varOrdinal: number[]; // field index -> its place among the variable fields, -1 if fixed
  varCount: number;
}

/** A raw value: what the record holds, before any conversion. */
export type Raw = number | bigint | boolean | string | Uint8Array | [number, number];
/** A physical value: a raw one after its field's conversion. */
export type Value = Raw;

/** A frame the reader decoded and refused, with why. The read goes on. */
export interface Refusal {
  frameType: number;
  stream: string;
  why: string;
}

/**
 * A run: one dataset within a stream, under conditions of its own (SPEC §6.5).
 *
 * A scope acquisition is one (§5.4): the samples are a stream on a uniform
 * axis, and everything needed to read them back as what the instrument
 * measured — the volts a division the conversion used, the timebase the axis
 * came from, where the trigger is — is in the run's parameters.
 */
export interface Run {
  id: number;
  index: number;
  params: Record<string, string>;
}

export interface Handler {
  sync?(seq: bigint, wallTimeNs: bigint): void;
  schema?(s: Schema): void;
  run?(r: Run): void;
  meta?(key: string, value: string, streamId: number): void;
  attach?(name: string, data: Uint8Array): void;
  hold?(h: Hold): void;
  data?(b: Batch): void;
  end?(): void;
  refused?(r: Refusal): void;
}

class Corrupt extends Error {}

// ---- CRC-32C, the checksum on every frame (SPEC §3.2) ----

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0x82f63b78 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

/** crc32c continues a checksum, as Go's crc32.Update does: pass the last result. */
export function crc32c(b: Uint8Array, crc = 0): number {
  let c = ~crc >>> 0;
  for (let i = 0; i < b.length; i++) c = CRC_TABLE[(c ^ b[i]) & 0xff] ^ (c >>> 8);
  return ~c >>> 0;
}

const utf8 = new TextDecoder("utf-8");

// ---- payload decoding ----

class Dec {
  b: Uint8Array;
  dv: DataView;
  off = 0;

  constructor(b: Uint8Array) {
    this.b = b;
    this.dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  }
  need(n: number) {
    if (this.off + n > this.b.length) throw new Corrupt("payload ends early");
  }
  u8() {
    this.need(1);
    return this.b[this.off++];
  }
  i8() {
    this.need(1);
    return this.dv.getInt8(this.off++);
  }
  u16() {
    this.need(2);
    const v = this.dv.getUint16(this.off, true);
    this.off += 2;
    return v;
  }
  u32() {
    this.need(4);
    const v = this.dv.getUint32(this.off, true);
    this.off += 4;
    return v;
  }
  u64() {
    this.need(8);
    const v = this.dv.getBigUint64(this.off, true);
    this.off += 8;
    return v;
  }
  f64() {
    this.need(8);
    const v = this.dv.getFloat64(this.off, true);
    this.off += 8;
    return v;
  }
  bytes(n: number) {
    this.need(n);
    const v = this.b.subarray(this.off, this.off + n);
    this.off += n;
    return v;
  }
  str() {
    return utf8.decode(this.bytes(this.u32()));
  }
  kv() {
    const n = this.u32();
    const m: Record<string, string> = {};
    for (let i = 0; i < n; i++) {
      const k = this.str();
      m[k] = this.str();
    }
    return m;
  }
}

/** An 8-byte axis value: ticks on a time axis, a float on any other. */
function axisVal(bits: bigint, time: boolean): bigint | number {
  if (time) return BigInt.asIntN(64, bits);
  const f = new Float64Array(1);
  new BigUint64Array(f.buffer)[0] = bits;
  return f[0];
}

function decodeConv(d: Dec): Conversion {
  const kind = d.u8();
  switch (kind) {
    case 0:
      return { kind: "identity" };
    case 1:
      return { kind: "linear", a: d.f64(), b: d.f64() };
    case 2:
      return { kind: "rational", p: [d.f64(), d.f64(), d.f64(), d.f64(), d.f64(), d.f64()] };
    case 3:
    case 4: {
      const n = d.u32();
      const keys: number[] = [];
      const vals: number[] = [];
      for (let i = 0; i < n; i++) {
        keys.push(d.f64());
        vals.push(d.f64());
      }
      return { kind: "table", keys, vals, interp: kind === 4 };
    }
    case 5: {
      const n = d.u32();
      const keys: number[] = [];
      const texts: string[] = [];
      for (let i = 0; i < n; i++) {
        keys.push(d.f64());
        texts.push(d.str());
      }
      return { kind: "v2t", keys, texts, def: d.str() };
    }
    case 6: {
      const n = d.u32();
      const los: number[] = [];
      const his: number[] = [];
      const texts: string[] = [];
      for (let i = 0; i < n; i++) {
        los.push(d.f64());
        his.push(d.f64());
        texts.push(d.str());
      }
      return { kind: "r2t", los, his, texts, def: d.str() };
    }
  }
  // No length to skip by: a conversion outside 0..6 is a corrupt schema.
  throw new Corrupt(`conversion type ${kind}`);
}

function decodeField(d: Dec): Field {
  const name = d.str();
  const bitOffset = d.u32();
  const bitWidth = d.u32();
  const type = d.u8();
  const bigEndian = d.u8() === 1;
  const flags = d.u8();
  const unit = d.str();
  const desc = d.str();
  const conv = decodeConv(d);
  let guardField = -1;
  let guardValue = 0n;
  if (flags & 2) {
    guardField = d.u16();
    guardValue = d.u64();
  }
  const meta = d.kv();
  return {
    name, bitOffset, bitWidth, type, bigEndian, unit, desc, conv, guardField, guardValue, meta,
    variable: (flags & 1) !== 0,
    guarded: (flags & 2) !== 0,
    held: (flags & 4) !== 0,
    guardNum: guardValue <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(guardValue) : NaN,
  };
}

function hex(b: Uint8Array) {
  let s = "";
  for (const x of b) s += x.toString(16).padStart(2, "0");
  return s;
}

function decodeSchema(streamId: number, p: Uint8Array): Schema {
  const d = new Dec(p);
  const uuid = hex(d.bytes(16));
  const name = d.str();
  const recordBits = d.u32();
  const axisKind = d.u8();
  const axisMode = d.u8();
  const axisExp = d.i8();
  d.u8();
  const axisUnit = d.str();
  const time = axisKind === AxisKind.Time;
  const axisStep = axisVal(d.u64(), time);
  const axisScale = axisVal(d.u64(), time);
  const axisField = d.u16();
  const n = d.u16();
  const fields: Field[] = [];
  for (let i = 0; i < n; i++) fields.push(decodeField(d));
  const meta = d.kv();

  const varOrdinal: number[] = [];
  let varCount = 0;
  for (const f of fields) varOrdinal.push(f.variable ? varCount++ : -1);
  for (const f of fields) {
    if (f.guarded && (f.guardField < 0 || f.guardField >= fields.length)) {
      throw new Corrupt(`field ${f.name} guards on field ${f.guardField}, which does not exist`);
    }
    // §7: the schema is refused, not the value.
    if ((f.type === DataType.Bytes || f.type === DataType.String) && f.conv.kind !== "identity") {
      throw new Corrupt(`field ${f.name}: a ${f.conv.kind} conversion on text or bytes`);
    }
  }
  return {
    streamId, uuid, name, recordBits, recordBytes: Math.ceil(recordBits / 8), axisKind, axisMode, axisExp,
    axisUnit, axisStep, axisScale, axisField, fields, meta, varOrdinal, varCount,
  };
}

// ---- bits (SPEC §6.2) ----
//
// Both byte orders name a field's first bit with its offset and run upward;
// they differ only in where a byte's bits start being counted — from the LSB
// for little-endian, from the MSB for big-endian, which is DBC's Motorola.

function outOfRecord(rec: Uint8Array, off: number, width: number) {
  if (width === 0 || width > 64 || off + width > rec.length * 8) {
    throw new Corrupt(`bits ${off}..${off + width} of a ${rec.length}-byte record`);
  }
}

/** bitsNum extracts up to 32 bits as a number. */
function bitsNum(rec: Uint8Array, off: number, width: number, bigEndian: boolean): number {
  outOfRecord(rec, off, width);
  const o = Math.floor(off / 8);
  const sh = off % 8;
  const nb = Math.ceil((sh + width) / 8); // at most 5 bytes: exact in a number
  let acc = 0;
  if (!bigEndian) {
    for (let k = nb - 1; k >= 0; k--) acc = acc * 256 + rec[o + k];
    return Math.floor(acc / 2 ** sh) % 2 ** width;
  }
  for (let k = 0; k < nb; k++) acc = acc * 256 + rec[o + k];
  return Math.floor(acc / 2 ** (nb * 8 - sh - width)) % 2 ** width;
}

/** bitsBig extracts up to 64 bits as a bigint. */
function bitsBig(rec: Uint8Array, off: number, width: number, bigEndian: boolean): bigint {
  outOfRecord(rec, off, width);
  const o = Math.floor(off / 8);
  const sh = off % 8;
  const nb = Math.ceil((sh + width) / 8);
  const mask = (1n << BigInt(width)) - 1n;
  let acc = 0n;
  if (!bigEndian) {
    for (let k = nb - 1; k >= 0; k--) acc = (acc << 8n) | BigInt(rec[o + k]);
    return (acc >> BigInt(sh)) & mask;
  }
  for (let k = 0; k < nb; k++) acc = (acc << 8n) | BigInt(rec[o + k]);
  return (acc >> BigInt(nb * 8 - sh - width)) & mask;
}

const B32 = 2 ** 32;

function exact(v: bigint): number | bigint {
  return v >= BigInt(Number.MIN_SAFE_INTEGER) && v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v;
}

function uintAt(rec: Uint8Array, f: Field): number | bigint {
  if (f.bitWidth <= 32) return bitsNum(rec, f.bitOffset, f.bitWidth, f.bigEndian);
  // The common wide case — a byte-aligned little-endian 64-bit value, which is
  // every time axis ktestd writes — without a bigint, when a number holds it.
  if (f.bitWidth === 64 && !f.bigEndian && f.bitOffset % 8 === 0) {
    const o = f.bitOffset / 8;
    outOfRecord(rec, f.bitOffset, 64);
    const hi = rec[o + 4] + rec[o + 5] * 256 + rec[o + 6] * 65536 + rec[o + 7] * 16777216;
    if (hi < 2 ** 21) return hi * B32 + (rec[o] + rec[o + 1] * 256 + rec[o + 2] * 65536 + rec[o + 3] * 16777216);
  }
  return exact(bitsBig(rec, f.bitOffset, f.bitWidth, f.bigEndian));
}

function sintAt(rec: Uint8Array, f: Field): number | bigint {
  const w = f.bitWidth;
  if (w <= 32) {
    const v = bitsNum(rec, f.bitOffset, w, f.bigEndian);
    return v >= 2 ** (w - 1) ? v - 2 ** w : v;
  }
  if (w === 64 && !f.bigEndian && f.bitOffset % 8 === 0) {
    const o = f.bitOffset / 8;
    outOfRecord(rec, f.bitOffset, 64);
    let hi = rec[o + 4] + rec[o + 5] * 256 + rec[o + 6] * 65536 + rec[o + 7] * 16777216;
    if (hi >= 2 ** 31) hi -= B32;
    if (hi > -(2 ** 21) && hi < 2 ** 21) {
      return hi * B32 + (rec[o] + rec[o + 1] * 256 + rec[o + 2] * 65536 + rec[o + 3] * 16777216);
    }
  }
  return exact(BigInt.asIntN(w, bitsBig(rec, f.bitOffset, w, f.bigEndian)));
}

const f32 = new Float32Array(1);
const u32 = new Uint32Array(f32.buffer);
const f64 = new Float64Array(1);
const u64 = new BigUint64Array(f64.buffer);

function floatBits(rec: Uint8Array, off: number, width: number, bigEndian: boolean): number {
  switch (width) {
    case 16:
      return half(bitsNum(rec, off, 16, bigEndian));
    case 32:
      u32[0] = bitsNum(rec, off, 32, bigEndian);
      return f32[0];
    case 64:
      u64[0] = bitsBig(rec, off, 64, bigEndian);
      return f64[0];
  }
  throw new Corrupt(`a ${width}-bit float`);
}

/** half is an IEEE 754 binary16, which SPEC §6.3 allows for a float field. */
function half(h: number): number {
  const s = h & 0x8000 ? -1 : 1;
  const e = (h >> 10) & 0x1f;
  const m = h & 0x3ff;
  if (e === 0) return s * m * 2 ** -24;
  if (e === 31) return m ? NaN : s * Infinity;
  return s * (1 + m / 1024) * 2 ** (e - 15);
}

/**
 * fieldRaw is field f of one record, before conversion, or undefined when a
 * guard says the record does not carry it — absent, never zero (SPEC §6.2).
 * tail is the record's variable-length values, in declaration order.
 */
export function fieldRaw(s: Schema, rec: Uint8Array, tail: Uint8Array[] | undefined, f: number): Raw | undefined {
  const fd = s.fields[f];
  if (fd.guarded) {
    const g = s.fields[fd.guardField];
    const w = g.type === DataType.Bool ? 1 : g.bitWidth; // a bool is one bit however spelled
    const ok = w <= 32 ? bitsNum(rec, g.bitOffset, w, g.bigEndian) === fd.guardNum
      : bitsBig(rec, g.bitOffset, w, g.bigEndian) === fd.guardValue;
    if (!ok) return undefined;
  }
  if (fd.variable) {
    const blob = tail?.[s.varOrdinal[f]];
    if (blob === undefined) throw new Corrupt(`${fd.name}: no tail value`);
    if (fd.type === DataType.String) return utf8.decode(blob);
    if (fd.type === DataType.Bytes) return blob.slice();
    throw new Corrupt(`${fd.name}: a variable field that is neither bytes nor a string`);
  }
  switch (fd.type) {
    case DataType.Bytes:
    case DataType.String: {
      if (fd.bitOffset % 8 !== 0 || fd.bitWidth % 8 !== 0) throw new Corrupt(`${fd.name}: an unaligned blob`);
      const o = fd.bitOffset / 8;
      const n = fd.bitWidth / 8;
      if (o + n > rec.length) throw new Corrupt(`${fd.name}: past the record`);
      const b = rec.subarray(o, o + n);
      if (fd.type === DataType.Bytes) return b.slice();
      // A fixed string is zero-padded, and ends at the first zero byte.
      const z = b.indexOf(0);
      return utf8.decode(z >= 0 ? b.subarray(0, z) : b);
    }
    case DataType.Bool:
      return bitsNum(rec, fd.bitOffset, 1, fd.bigEndian) !== 0;
    case DataType.Uint:
      return uintAt(rec, fd);
    case DataType.Sint:
      return sintAt(rec, fd);
    case DataType.Float:
      return floatBits(rec, fd.bitOffset, fd.bitWidth, fd.bigEndian);
    case DataType.Complex: {
      const h = fd.bitWidth / 2;
      if (h !== 32 && h !== 64) throw new Corrupt(`${fd.name}: a ${fd.bitWidth}-bit complex`);
      return [floatBits(rec, fd.bitOffset, h, fd.bigEndian), floatBits(rec, fd.bitOffset + h, h, fd.bigEndian)];
    }
  }
  throw new Corrupt(`${fd.name}: data type ${fd.type}`);
}

/** applyConv converts a raw number, as logb's Conversion.Apply does. */
export function applyConv(c: Conversion, x: number): number | string {
  switch (c.kind) {
    case "identity":
      return x;
    case "linear":
      return c.a + c.b * x;
    case "rational": {
      const [p1, p2, p3, p4, p5, p6] = c.p;
      return (p1 * x * x + p2 * x + p3) / (p4 * x * x + p5 * x + p6);
    }
    case "table": {
      const { keys, vals } = c;
      if (keys.length === 0) return x;
      if (x <= keys[0]) return vals[0];
      const last = keys.length - 1;
      if (x >= keys[last]) return vals[last];
      let i = 0;
      while (i < last && keys[i + 1] <= x) i++;
      if (!c.interp) return vals[i];
      const span = keys[i + 1] - keys[i];
      if (span === 0) return vals[i];
      return vals[i] + ((x - keys[i]) / span) * (vals[i + 1] - vals[i]);
    }
    case "v2t": {
      const i = c.keys.indexOf(x);
      return i >= 0 ? c.texts[i] : c.def;
    }
    case "r2t":
      for (let i = 0; i < c.los.length; i++) if (x >= c.los[i] && x <= c.his[i]) return c.texts[i];
      return c.def;
  }
}

/**
 * fieldValue is field f of one record after its conversion. Identity keeps the
 * value's type (§7): only a conversion that is present makes a number of it.
 */
export function fieldValue(s: Schema, rec: Uint8Array, tail: Uint8Array[] | undefined, f: number): Value | undefined {
  const raw = fieldRaw(s, rec, tail, f);
  if (raw === undefined) return undefined;
  const c = s.fields[f].conv;
  if (c.kind === "identity") return raw;
  if (Array.isArray(raw)) {
    // Complex: linear and rational apply component-wise with real coefficients,
    // as complex(a, 0) + complex(b, 0)*z does in Go.
    const [re, im] = raw;
    if (c.kind === "linear") return [c.a + (c.b * re - 0 * im), c.b * im + 0 * re];
    if (c.kind === "rational") {
      const [p1, p2, p3, p4, p5, p6] = c.p;
      const zz: [number, number] = [re * re - im * im, 2 * re * im];
      const num: [number, number] = [p1 * zz[0] + p2 * re + p3, p1 * zz[1] + p2 * im];
      const den: [number, number] = [p4 * zz[0] + p5 * re + p6, p4 * zz[1] + p5 * im];
      const d = den[0] * den[0] + den[1] * den[1];
      return [(num[0] * den[0] + num[1] * den[1]) / d, (num[1] * den[0] - num[0] * den[1]) / d];
    }
    throw new Corrupt(`${s.fields[f].name}: a ${c.kind} conversion on a complex value`);
  }
  let x: number;
  if (typeof raw === "number") x = raw;
  else if (typeof raw === "bigint") x = Number(raw);
  else if (typeof raw === "boolean") x = raw ? 1 : 0;
  else return raw;
  return applyConv(c, x);
}

/** A DATA frame's records, decoded and ready to read. */
export class Batch {
  schema: Schema;
  axisBase: bigint | number;
  runId: number;
  /**
   * The run these records belong to, when the segment declared one: for a scope
   * acquisition it carries the settings the samples were taken under (§5.4).
   * Undefined when no RUN frame named this id, which is the ordinary case for a
   * logger.
   */
  run: Run | undefined;
  count: number;
  fixed: Uint8Array;
  tails: Uint8Array[][];

  constructor(schema: Schema, axisBase: bigint | number, runId: number, count: number, fixed: Uint8Array,
    tails: Uint8Array[][], run?: Run) {
    this.schema = schema;
    this.axisBase = axisBase;
    this.runId = runId;
    this.run = run;
    this.count = count;
    this.fixed = fixed;
    this.tails = tails;
  }

  record(i: number): Uint8Array {
    const n = this.schema.recordBytes;
    return this.fixed.subarray(i * n, (i + 1) * n);
  }
  raw(i: number, f: number): Raw | undefined {
    return fieldRaw(this.schema, this.record(i), this.tails[i], f);
  }
  value(i: number, f: number): Value | undefined {
    return fieldValue(this.schema, this.record(i), this.tails[i], f);
  }

  /** axis is record i's axis value: ticks on a time axis, as logb's AxisAt computes it. */
  axis(i: number): bigint | number {
    const s = this.schema;
    let explicit = 0;
    if (s.axisMode === AxisMode.Explicit) {
      const r = this.raw(i, s.axisField);
      explicit = typeof r === "number" ? r : typeof r === "bigint" ? Number(r) : r === true ? 1 : 0;
    }
    if (s.axisKind === AxisKind.Time) {
      const base = this.axisBase as bigint;
      if (s.axisMode === AxisMode.Implicit) return base + BigInt(i) * (s.axisStep as bigint);
      return base + BigInt(Math.trunc(explicit)) * (s.axisScale as bigint);
    }
    const base = this.axisBase as number;
    switch (s.axisMode) {
      case AxisMode.Implicit:
        return base + i * (s.axisStep as number);
      case AxisMode.Log:
        return base * Math.pow(s.axisStep as number, i);
    }
    return base + explicit * (s.axisScale as number);
  }

  /**
   * seconds is record i's axis in the axis's unit — seconds, on a time axis. It
   * is the live path's version of axis: a number, and no bigint on the way,
   * exact while the tick count stays below 2^53, which on a nanosecond axis is
   * a hundred days from the recording's start.
   */
  seconds(i: number): number {
    const s = this.schema;
    if (s.axisKind !== AxisKind.Time) return this.axis(i) as number;
    const scale = 10 ** s.axisExp;
    const base = Number(this.axisBase);
    if (s.axisMode === AxisMode.Implicit) return (base + i * Number(s.axisStep)) * scale;
    const r = this.raw(i, s.axisField);
    const x = typeof r === "number" ? r : Number(r);
    return (base + Math.trunc(x) * Number(s.axisScale)) * scale;
  }
}

/** A HOLD frame: a stream's held values as of a segment boundary (SPEC §6.10). */
export class Hold {
  schema: Schema;
  axisBase: bigint | number;
  runId: number;
  present: boolean[];
  record: Uint8Array;
  tail: Uint8Array[];

  constructor(schema: Schema, axisBase: bigint | number, runId: number, present: boolean[], record: Uint8Array,
    tail: Uint8Array[]) {
    this.schema = schema;
    this.axisBase = axisBase;
    this.runId = runId;
    this.present = present;
    this.record = record;
    this.tail = tail;
  }

  /** value is field f's restated value, or undefined when the frame does not restate it. */
  value(f: number): Value | undefined {
    if (!this.present[f]) return undefined;
    return fieldValue(this.schema, this.record, this.tail, f);
  }
}

function detranspose(data: Uint8Array, recSize: number): Uint8Array {
  if (recSize <= 1 || data.length % recSize !== 0) return data;
  const n = data.length / recSize;
  const out = new Uint8Array(data.length);
  let k = 0;
  for (let c = 0; c < recSize; c++) for (let r = 0; r < n; r++) out[r * recSize + c] = data[k++];
  return out;
}

function readTail(d: Dec, count: number): Uint8Array[] {
  const t: Uint8Array[] = [];
  for (let k = 0; k < count; k++) t.push(d.bytes(d.u32()));
  return t;
}

/**
 * Reader decodes a Logb byte stream pushed to it in pieces of any size.
 *
 * Damage ends the read (SPEC §3.2): a CRC that does not match, or a frame that
 * cannot be parsed, sets broken and nothing after it is decoded. A frame the
 * reader understands and will not decode — an unknown codec — is refused and
 * reported, and the read goes on.
 */
export class Reader {
  broken = "";
  frames = 0;

  private h: Handler;
  private buf = new Uint8Array(0);
  private pos = 0;
  private headerDone = false;
  private schemas = new Map<number, Schema>();
  private runs = new Map<number, Run>();

  constructor(h: Handler) {
    this.h = h;
  }

  push(chunk: Uint8Array) {
    if (this.broken) return;
    const rest = this.buf.length - this.pos;
    const b = new Uint8Array(rest + chunk.length);
    b.set(this.buf.subarray(this.pos));
    b.set(chunk, rest);
    this.buf = b;
    this.pos = 0;

    if (!this.headerDone) {
      if (b.length < 16) return;
      for (let i = 0; i < 8; i++) if (b[i] !== MAGIC[i]) return this.fail("not a Logb stream");
      const dv = new DataView(b.buffer, b.byteOffset, 16);
      if (dv.getUint32(12, true) !== crc32c(b.subarray(0, 12))) return this.fail("file header CRC");
      if (dv.getUint16(8, true) !== 0) return this.fail(`version ${dv.getUint16(8, true)}, and this reader knows 0`);
      this.headerDone = true;
      this.pos = 16;
    }

    const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
    while (b.length - this.pos >= 8) {
      const len = dv.getUint32(this.pos, true);
      const total = 8 + len + 4;
      if (b.length - this.pos < total) break;
      const hdr = b.subarray(this.pos, this.pos + 8);
      const payload = b.subarray(this.pos + 8, this.pos + 8 + len);
      if (dv.getUint32(this.pos + 8 + len, true) !== crc32c(payload, crc32c(hdr))) {
        return this.fail(`frame CRC at stream offset +${this.pos}`);
      }
      const type = hdr[4];
      const streamId = dv.getUint16(this.pos + 6, true);
      this.pos += total;
      this.frames++;
      try {
        this.frame(type, streamId, payload);
      } catch (e) {
        return this.fail(e instanceof Error ? e.message : String(e));
      }
    }
  }

  private fail(why: string) {
    this.broken = why;
  }

  private refuse(frameType: number, s: Schema | undefined, why: string) {
    this.h.refused?.({ frameType, stream: s?.name ?? "", why });
  }

  private frame(type: number, streamId: number, p: Uint8Array) {
    const h = this.h;
    switch (type) {
      case FrameType.Sync: {
        const d = new Dec(p);
        const pat = d.bytes(16);
        for (let i = 0; i < 16; i++) if (pat[i] !== SYNC_PATTERN[i]) throw new Corrupt("SYNC pattern");
        // A new segment rebinds every stream id (§6.6), so the old bindings go,
        // and with them the runs: a segment restates every one that is still in
        // force, so one kept across the boundary names a run this segment has
        // not declared.
        this.schemas.clear();
        this.runs.clear();
        const seq = d.u64();
        h.sync?.(seq, BigInt.asIntN(64, d.u64()));
        return;
      }
      case FrameType.Run: {
        const d = new Dec(p);
        const id = d.u32();
        const index = d.u32();
        const r: Run = { id, index, params: d.kv() };
        this.runs.set(id, r);
        h.run?.(r);
        return;
      }
      case FrameType.Schema: {
        const s = decodeSchema(streamId, p);
        this.schemas.set(streamId, s);
        h.schema?.(s);
        return;
      }
      case FrameType.Meta: {
        const d = new Dec(p);
        const k = d.str();
        h.meta?.(k, d.str(), streamId);
        return;
      }
      case FrameType.Attach: {
        const d = new Dec(p);
        const name = d.str();
        h.attach?.(name, d.bytes(d.u32()).slice());
        return;
      }
      case FrameType.Hold: {
        const s = this.schemas.get(streamId);
        if (!s) return; // unbound in this segment: skipped, as an unbound DATA frame is
        const d = new Dec(p);
        const base = axisVal(d.u64(), s.axisKind === AxisKind.Time);
        const runId = d.u32();
        d.u32();
        const bits = d.bytes(Math.ceil(s.fields.length / 8));
        const present = s.fields.map((_, i) => (bits[i >> 3] & (1 << (i & 7))) !== 0);
        const record = d.bytes(s.recordBytes).slice();
        h.hold?.(new Hold(s, base, runId, present, record, readTail(d, s.varCount)));
        return;
      }
      case FrameType.Data: {
        const s = this.schemas.get(streamId);
        if (!s) return;
        const d = new Dec(p);
        const base = axisVal(d.u64(), s.axisKind === AxisKind.Time);
        const count = d.u32();
        const runId = d.u32();
        const codec = d.u8();
        const filter = d.u8();
        d.u16();
        const rawSize = d.u64();
        const region = p.subarray(d.off);
        if (codec !== 0) {
          return this.refuse(type, s, `codec ${codec}; this reader decodes codec 0 (none) only`);
        }
        if (filter > 1) return this.refuse(type, s, `filter ${filter}`);
        if (BigInt(region.length) !== rawSize) {
          return this.refuse(type, s, `${region.length} bytes of records where the frame says ${rawSize}`);
        }
        const fixedLen = s.recordBytes * count;
        if (region.length < fixedLen) throw new Corrupt(`${s.name}: ${count} records in ${region.length} bytes`);
        let fixed = region.subarray(0, fixedLen);
        if (filter === 1) fixed = detranspose(fixed, s.recordBytes);
        const tails: Uint8Array[][] = [];
        if (s.varCount > 0) {
          const td = new Dec(region.subarray(fixedLen));
          for (let r = 0; r < count; r++) tails.push(readTail(td, s.varCount));
        }
        h.data?.(new Batch(s, base, runId, count, fixed, tails, this.runs.get(runId)));
        return;
      }
      case FrameType.End:
        h.end?.();
        return;
    }
    // RUN, INDEX, and any frame type this reader does not know: skipped by its
    // length, which is the format's one extension mechanism (§4.2).
  }
}
