// The page's side of the stream: per-channel sample rings, the latest values,
// and the stream's status. Widgets read it; only the worker's messages write it.

import type { Acquisition, ChannelInfo, FromWorker, InstrumentInfo, Latest, Status, ToWorker } from "./messages.ts";

/**
 * A Series is a channel's recent samples in a mirrored ring: every sample is
 * written twice, cap apart, so the latest n are always one contiguous slice.
 * A plot is handed subarrays of the same two buffers every frame, and nothing
 * is allocated on the way (§10.3).
 */
export class Series {
  readonly cap: number;
  readonly t: Float64Array;
  readonly v: Float64Array;
  private head = 0;
  private n = 0;

  constructor(cap: number) {
    this.cap = cap;
    this.t = new Float64Array(2 * cap);
    this.v = new Float64Array(2 * cap);
  }

  push(t: number, v: number) {
    const i = this.head;
    this.t[i] = this.t[i + this.cap] = t;
    this.v[i] = this.v[i + this.cap] = v;
    this.head = (i + 1) % this.cap;
    if (this.n < this.cap) this.n++;
  }

  /** view is the samples held, oldest first. */
  view(): { t: Float64Array; v: Float64Array } {
    const start = (this.head - this.n + this.cap) % this.cap;
    return { t: this.t.subarray(start, start + this.n), v: this.v.subarray(start, start + this.n) };
  }

  clear() {
    this.head = this.n = 0;
  }
}

/** lowerBound is the first index whose value is at least x, in a sorted array. */
export function lowerBound(a: Float64Array, x: number): number {
  let lo = 0;
  let hi = a.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (a[mid] < x) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

export class Store {
  channels = new Map<string, ChannelInfo>();
  /** Every instrument the stream carries a control stream for, and its kind. */
  instruments = new Map<string, InstrumentInfo>();
  series = new Map<string, Series>();
  latest = new Map<string, Latest>();

  /**
   * The newest acquisition of each scope channel, by the channel it reduces:
   * "scope1.ch1". A waveform is a picture taken again rather than a series that
   * grows, so there is one, and it is replaced whole.
   */
  acqs = new Map<string, Acquisition>();
  /** acqVersion changes whenever any acquisition does, so a widget can tell. */
  acqVersion = 0;
  status: Status = { state: "connecting", detail: "", bytes: 0, refused: 0 };

  /** The last panel the stream carried, its sha256, and a count of arrivals. */
  panel?: string;
  panelSha = "";
  panelVersion = 0;

  /** version changes whenever anything a widget draws from does. */
  version = 0;
  channelsVersion = 0;

  private worker: Worker;
  private subs = new Set<string>();

  constructor(token: string) {
    this.worker = new Worker(new URL("./stream.worker.ts", import.meta.url), { type: "module" });
    this.worker.onmessage = (e: MessageEvent<FromWorker>) => this.receive(e.data);
    const start: ToWorker = { type: "start", url: new URL("stream/rack", location.href).href, token };
    this.worker.postMessage(start);
  }

  /** subscribe asks for every sample of these channels, not only the latest. */
  subscribe(names: string[]) {
    for (const n of names) {
      this.subs.add(n);
      if (!this.series.has(n)) this.series.set(n, new Series(65536));
    }
    const m: ToWorker = { type: "subscribe", channels: [...this.subs] };
    this.worker.postMessage(m);
  }

  private receive(m: FromWorker) {
    switch (m.type) {
      case "channels":
        for (const c of m.channels) this.channels.set(c.name, c);
        for (const i of m.instruments) this.instruments.set(i.name, i);
        this.channelsVersion++;
        break;
      case "acq":
        this.acqs.set(m.acq.of, m.acq);
        this.acqVersion++;
        break;
      case "data":
        for (const [name, s] of Object.entries(m.series)) {
          const ring = this.series.get(name);
          if (!ring) continue;
          for (let i = 0; i < s.t.length; i++) ring.push(s.t[i], s.v[i]);
        }
        for (const [name, v] of Object.entries(m.latest)) this.latest.set(name, v);
        break;
      case "status":
        this.status = m.status;
        break;
      case "panel":
        this.panel = m.text;
        this.panelSha = m.sha;
        this.panelVersion++;
        break;
      case "reset":
        for (const s of this.series.values()) s.clear();
        this.acqs.clear();
        break;
    }
    this.version++;
  }
}
