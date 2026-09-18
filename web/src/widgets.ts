// The widgets. Each owns its rendering surface: Lit draws its frame once, and
// data reaches it only through draw(), which writes into the surface directly
// (§10.1). The rack calls draw() from its one animation frame loop.

import { LitElement, html, svg } from "lit";
import uPlot from "uplot";
import { lowerBound, type Store } from "./store.ts";
import type { Acquisition, Latest } from "./messages.ts";
import type { Control } from "./control.ts";

export interface Drawable extends HTMLElement {
  draw(): void;
}

const COLORS = ["#4e79a7", "#f28e2b", "#e15759", "#76b7b2", "#59a14f", "#edc948", "#b07aa1", "#9c755f"];

export function format(v: Latest | undefined): string {
  if (v === undefined) return "—";
  if (typeof v === "number") {
    if (Number.isInteger(v)) return String(v);
    return String(Number(v.toPrecision(6)));
  }
  return String(v);
}

/** A readout: one channel's latest value, and its unit. */
export class ReadoutElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String } };
  declare channel: string;
  declare label: string;
  declare store: Store;

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <div class="value">—</div>
      <div class="unit"></div>`;
  }

  draw() {
    const value = this.querySelector(".value");
    const unit = this.querySelector(".unit");
    if (!value || !unit) return;
    const info = this.store.channels.get(this.channel);
    this.classList.toggle("unresolved", !info);
    value.textContent = format(this.store.latest.get(this.channel));
    unit.textContent = info ? info.unit : "no such channel yet";
  }
}

/**
 * A plot: one or more channels over a moving window. Channels from different
 * streams have different timestamps; they are drawn on the union of them, each
 * holding its last value until its next sample — the step a held or sampled
 * channel really is — so the arrays uPlot gets have no gaps to express (§10.3).
 */
export class PlotElement extends LitElement implements Drawable {
  static properties = { channels: { type: Array }, window: { type: Number }, label: { type: String } };
  declare channels: string[];
  declare window: number;
  declare label: string;
  declare store: Store;

  private plot?: uPlot;
  private resize?: ResizeObserver;
  // Merge buffers, grown as needed and reused every frame.
  private mx = new Float64Array(0);
  private my: Float64Array[] = [];

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channels.join(", ")}</div><div class="chart"></div>`;
  }

  firstUpdated() {
    const el = this.querySelector(".chart") as HTMLElement;
    const size = () => ({ width: Math.max(80, el.clientWidth), height: Math.max(60, el.clientHeight) });
    this.plot = new uPlot(
      {
        ...size(),
        scales: { x: { time: false } },
        legend: { show: this.channels.length > 1 },
        cursor: { drag: { x: false, y: false } },
        series: [{ label: "s" }, ...this.channels.map((c, i) => ({ label: c, stroke: COLORS[i % COLORS.length], width: 1.5 }))],
        axes: [{ stroke: "#9aa4b2", grid: { stroke: "#2a3140" } }, { stroke: "#9aa4b2", grid: { stroke: "#2a3140" } }],
      },
      [[], ...this.channels.map(() => [])],
      el,
    );
    this.resize = new ResizeObserver(() => this.plot?.setSize(size()));
    this.resize.observe(el);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.resize?.disconnect();
    this.plot?.destroy();
  }

  draw() {
    if (!this.plot) return;
    const views = this.channels.map((c) => this.store.series.get(c)?.view());
    // Until every channel has a value there is nothing honest to draw.
    if (views.some((v) => !v || v.t.length === 0)) return;
    const vs = views as { t: Float64Array; v: Float64Array }[];
    const end = Math.max(...vs.map((v) => v.t[v.t.length - 1]));
    const from = end - (this.window || 10);

    if (vs.length === 1) {
      const lo = lowerBound(vs[0].t, from);
      this.plot.setData([vs[0].t.subarray(lo), vs[0].v.subarray(lo)] as unknown as uPlot.AlignedData);
      return;
    }
    this.plot.setData(this.merge(vs, from) as unknown as uPlot.AlignedData);
  }

  // merge lays the channels on the union of their timestamps from `from` on,
  // each carrying its last value forward. It starts where every channel has a
  // value, so no channel is drawn from before it existed.
  private merge(vs: { t: Float64Array; v: Float64Array }[], from: number): Float64Array[] {
    let begin = from;
    for (const v of vs) begin = Math.max(begin, v.t[0]);
    const idx: number[] = [];
    const cur: number[] = [];
    let total = 1;
    for (const v of vs) {
      const i = lowerBound(v.t, begin);
      const at = i < v.t.length && v.t[i] === begin ? i + 1 : i; // samples at begin are taken now
      cur.push(v.v[Math.max(0, at - 1)]);
      idx.push(at);
      total += v.t.length - at;
    }
    if (this.mx.length < total) {
      this.mx = new Float64Array(total * 2);
      this.my = vs.map(() => new Float64Array(total * 2));
    }
    let n = 0;
    this.mx[n] = begin;
    for (let k = 0; k < vs.length; k++) this.my[k][n] = cur[k];
    n++;
    for (;;) {
      let next = Infinity;
      for (let k = 0; k < vs.length; k++) if (idx[k] < vs[k].t.length) next = Math.min(next, vs[k].t[idx[k]]);
      if (next === Infinity) break;
      for (let k = 0; k < vs.length; k++) {
        while (idx[k] < vs[k].t.length && vs[k].t[idx[k]] === next) cur[k] = vs[k].v[idx[k]++];
        this.my[k][n] = cur[k];
      }
      this.mx[n++] = next;
    }
    return [this.mx.subarray(0, n), ...this.my.map((y) => y.subarray(0, n))];
  }
}

/**
 * A scope: one acquisition of one channel, drawn as the min/max band its
 * envelope is, with the settings it was taken under beside it and the trigger
 * marked (§12, milestone 4 slice 2).
 *
 * It is a band and not a line, and that is the whole point of §10.4: the daemon
 * reduced a megapoint acquisition to three thousand columns by keeping the
 * least and greatest sample in each, so a one-sample spike is still on the
 * screen. A line through the middle of each bucket would have thrown away
 * exactly what the reduction was built to keep, and would look perfectly
 * plausible doing it.
 *
 * Nothing is merged or carried forward here, as a plot does for channels on
 * different clocks: an acquisition is one instrument's samples on one uniform
 * axis, arriving whole, and it replaces its predecessor rather than extending
 * it. A bucket that held no sample is NaN, which uPlot breaks the trace at —
 * the gap is a fact about the acquisition and is drawn as one.
 */
export class ScopeElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String } };
  declare channel: string;
  declare label: string;
  declare store: Store;

  private plot?: uPlot;
  private resize?: ResizeObserver;
  private seen = -1;
  private run = -1;

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <div class="chart"></div>
      <div class="detail"></div>`;
  }

  firstUpdated() {
    const el = this.querySelector(".chart") as HTMLElement;
    const size = () => ({ width: Math.max(80, el.clientWidth), height: Math.max(60, el.clientHeight) });
    this.plot = new uPlot(
      {
        ...size(),
        scales: { x: { time: false } },
        legend: { show: false },
        cursor: { drag: { x: false, y: false } },
        series: [
          { label: "s" },
          // The band is two series with the space between them filled. Drawing
          // it as one line with a width would put the ink in the right place
          // for the wrong reason, and would not survive a zoom.
          // No fill on the series itself: uPlot fills a series down to the
          // axis, which would shade everything between the trace and 0 V and
          // make a band that is a sliver look like a solid area. The band's
          // fill is the bands entry below, between max and min and nowhere else.
          { label: "min", stroke: COLORS[0], width: 1, spanGaps: false },
          { label: "max", stroke: COLORS[0], width: 1, spanGaps: false },
        ],
        bands: [{ series: [2, 1], fill: "rgba(78,121,167,0.35)" }],
        axes: [{ stroke: "#9aa4b2", grid: { stroke: "#2a3140" } }, { stroke: "#9aa4b2", grid: { stroke: "#2a3140" } }],
        hooks: {
          // The trigger, at t = 0 by construction: the daemon writes the
          // envelope's axis relative to where it placed the acquisition, and
          // the worker subtracts that again. Marking it matters because half
          // the screen is before it, and a trace with no mark says nothing
          // about which half is which.
          draw: [
            (u) => {
              const x = u.valToPos(0, "x", true);
              if (!Number.isFinite(x)) return;
              const ctx = u.ctx;
              ctx.save();
              ctx.strokeStyle = "#e15759";
              ctx.setLineDash([4, 3]);
              ctx.beginPath();
              ctx.moveTo(x, u.bbox.top);
              ctx.lineTo(x, u.bbox.top + u.bbox.height);
              ctx.stroke();
              ctx.restore();
            },
          ],
        },
      },
      [[], [], []],
      el,
    );
    this.resize = new ResizeObserver(() => this.plot?.setSize(size()));
    this.resize.observe(el);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.resize?.disconnect();
    this.plot?.destroy();
  }

  draw() {
    if (!this.plot) return;
    const detail = this.querySelector(".detail");
    const a = this.store.acqs.get(this.channel);
    this.classList.toggle("unresolved", !a);
    if (!a) {
      if (detail) detail.textContent = "no acquisition yet";
      return;
    }
    // Only when there is a new one: redrawing the same picture every frame
    // costs a full uPlot pass for nothing.
    if (this.seen === this.store.acqVersion && this.run === a.run) return;
    this.seen = this.store.acqVersion;
    this.run = a.run;
    this.plot.setData([a.t, a.min, a.max] as unknown as uPlot.AlignedData);
    if (detail) detail.textContent = settingsOf(a);
  }
}

/**
 * settingsOf is the settings an acquisition was taken under, as the instrument
 * states them: what a person reads off the front panel, beside the trace that
 * came from it. They are the run's own parameters (§5.4), so they describe this
 * acquisition and not whatever the scope holds by the time it is drawn.
 */
function settingsOf(a: Acquisition): string {
  const p = a.params;
  const parts: string[] = [];
  if (p["vdiv"]) parts.push(`${si(Number(p["vdiv"]), "V")}/div`);
  if (p["tdiv"]) parts.push(`${si(Number(p["tdiv"]), "s")}/div`);
  if (p["ofst"] && Number(p["ofst"]) !== 0) parts.push(`offset ${si(Number(p["ofst"]), "V")}`);
  if (p["sara"]) parts.push(`${si(Number(p["sara"]), "Sa/s")}`);
  if (p["points"]) parts.push(`${p["points"]} pts`);
  // The scope's own account of when it triggered, on its own clock, which is
  // not the recording's and whose origin is not documented. It is shown beside
  // the acquisition's place on the recording's timeline and never in place of
  // it (§11).
  if (p["frame_time"]) parts.push(`scope clock ${p["frame_time"].replace(/ /g, "")}`);
  parts.push(`at ${a.at.toFixed(3)}s`);
  return parts.join(" · ");
}

/** si writes a value with the prefix an instrument's front panel would use. */
function si(v: number, unit: string): string {
  if (!Number.isFinite(v)) return `—${unit}`;
  const abs = Math.abs(v);
  const table: [number, string][] = [
    [1e9, "G"], [1e6, "M"], [1e3, "k"], [1, ""], [1e-3, "m"], [1e-6, "µ"], [1e-9, "n"], [1e-12, "p"],
  ];
  for (const [scale, prefix] of table) {
    if (abs >= scale) return `${Number((v / scale).toPrecision(4))} ${prefix}${unit}`;
  }
  return `${Number(v.toPrecision(4))} ${unit}`;
}

/** The channel table: every channel the stream carries, with its latest value. */
export class ChannelsElement extends LitElement implements Drawable {
  declare store: Store;
  private seen = -1;

  createRenderRoot() {
    return this;
  }

  render() {
    const names = [...(this.store?.channels.keys() ?? [])].sort();
    return html`<table>
      <thead><tr><th>channel</th><th>value</th><th>unit</th></tr></thead>
      <tbody>
        ${names.map((n) => {
          const c = this.store.channels.get(n)!;
          return html`<tr data-channel=${n}><td>${n}${c.held ? html` <span class="badge">held</span>` : ""}</td>
            <td class="value">—</td><td>${c.unit}</td></tr>`;
        })}
      </tbody>
    </table>`;
  }

  draw() {
    if (this.seen !== this.store.channelsVersion) {
      this.seen = this.store.channelsVersion;
      this.requestUpdate();
      return;
    }
    for (const row of this.querySelectorAll<HTMLElement>("tr[data-channel]")) {
      const cell = row.querySelector(".value");
      if (cell) cell.textContent = format(this.store.latest.get(row.dataset.channel!));
    }
  }
}

// The control widgets write a channel's set account and show its applied one
// beside it, never one in place of the other (§4): a supply may clamp what it
// was asked for, and the rack shows that it did.

/** applied is the account a set channel's instrument reports holding. */
function appliedOf(channel: string): string {
  return channel.replace(/\.set$/, ".applied");
}

function instrumentOf(channel: string): string {
  return channel.split(".")[0];
}

/** armed is the instrument's arming, as its control stream records it. */
function armed(store: Store, instrument: string): boolean {
  return store.latest.get(`${instrument}.control.armed`) === true;
}

/**
 * A setpoint: a number to send, and what the instrument holds. A value sent
 * is pending until the recording's set account carries it, which is the
 * daemon having taken it; what the instrument then holds is the applied account.
 */
export class SetpointElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String }, step: { type: Number } };
  declare channel: string;
  declare label: string;
  declare step: number;
  declare store: Store;
  declare control: Control;

  private pending?: number;
  private error = "";

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <div class="value">—</div>
      <div class="detail"></div>
      <form @submit=${this.submit}>
        <input type="number" min="0" step=${this.step ? String(this.step) : "any"} required />
        <button type="submit">Set</button>
      </form>`;
  }

  private async submit(e: Event) {
    e.preventDefault();
    const input = this.querySelector("input")!;
    const v = Number(input.value);
    if (input.value === "" || !Number.isFinite(v)) return;
    this.pending = v;
    this.error = "";
    const a = await this.control.write(this.channel, v);
    if (!a.ok) {
      this.pending = undefined;
      this.error = a.error ?? `HTTP ${a.status}`;
    }
    this.draw();
  }

  draw() {
    const value = this.querySelector(".value");
    const detail = this.querySelector(".detail");
    const input = this.querySelector("input");
    const button = this.querySelector("button");
    if (!value || !detail || !input || !button) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const set = this.store.latest.get(this.channel);
    const applied = this.store.latest.get(appliedOf(this.channel));
    if (this.pending !== undefined && set === this.pending) this.pending = undefined;
    const unit = info?.unit ?? "";
    const differs = typeof set === "number" && typeof applied === "number" && Math.abs(set - applied) > 1e-9;

    value.textContent = `${format(applied)} ${applied === undefined ? "" : unit}`;
    let d = info ? `set ${format(set)} ${set === undefined ? "" : unit}` : "no such channel yet";
    if (this.pending !== undefined) d = `sending ${format(this.pending)} ${unit}…`;
    else if (this.error) d = this.error;
    else if (differs) d += ` · the instrument holds ${format(applied)} ${unit}`;
    detail.textContent = d;

    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", this.pending !== undefined);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", differs && this.pending === undefined);
    const can = this.control.holds(instrumentOf(this.channel)) && armed(this.store, instrumentOf(this.channel));
    input.disabled = button.disabled = !can;
  }
}

/** A toggle: something switched on or off, showing what the instrument reports. */
export class ToggleElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String } };
  declare channel: string;
  declare label: string;
  declare store: Store;
  declare control: Control;

  private pending?: boolean;
  private error = "";

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <button class="switch" @click=${this.flip}>—</button>
      <div class="detail"></div>`;
  }

  private async flip() {
    const on = this.store.latest.get(appliedOf(this.channel)) === true;
    this.pending = !on;
    this.error = "";
    const a = await this.control.write(this.channel, !on);
    if (!a.ok) {
      this.pending = undefined;
      this.error = a.error ?? `HTTP ${a.status}`;
    }
    this.draw();
  }

  draw() {
    const button = this.querySelector<HTMLButtonElement>(".switch");
    const detail = this.querySelector(".detail");
    if (!button || !detail) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const set = this.store.latest.get(this.channel);
    const applied = this.store.latest.get(appliedOf(this.channel));
    if (this.pending !== undefined && set === this.pending) this.pending = undefined;
    const differs = typeof set === "boolean" && typeof applied === "boolean" && set !== applied;

    button.textContent = applied === undefined ? "—" : applied ? "ON" : "OFF";
    button.classList.toggle("on", applied === true);
    let d = info ? (set === undefined ? "not set since the daemon started" : `set ${set ? "on" : "off"}`) : "no such channel yet";
    if (this.pending !== undefined) d = `switching ${this.pending ? "on" : "off"}…`;
    else if (this.error) d = this.error;
    else if (differs) d += ` · the instrument reports ${applied ? "on" : "off"}`;
    detail.textContent = d;

    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", this.pending !== undefined);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", differs && this.pending === undefined);
    button.disabled = !(this.control.holds(instrumentOf(this.channel)) && armed(this.store, instrumentOf(this.channel)));
  }
}

/** A lamp: lit while a channel is true or non-zero, dark when not, hollow when unknown. */
export class LampElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String } };
  declare channel: string;
  declare label: string;
  declare store: Store;

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <div class="row"><span class="lamp"></span><span class="state">—</span></div>`;
  }

  draw() {
    const state = this.querySelector(".state");
    if (!state) return;
    const v = this.store.latest.get(this.channel);
    const on = v === true || (typeof v === "number" && v !== 0);
    this.classList.toggle("unresolved", !this.store.channels.has(this.channel));
    this.classList.toggle("unknown", v === undefined);
    this.classList.toggle("on", on);
    state.textContent = v === undefined ? "—" : on ? "on" : "off";
  }
}

/**
 * The control bar: the panel as one thing to take control of, arm and stop.
 *
 * The daemon leases and arms each instrument on its own (§7) and that has not
 * changed — a lease refused is still a lease refused, and two pages can still
 * drive two instruments. But a rack is one bench in front of one person, so
 * these take the panel's instruments together and say how many when they do
 * not agree. Which one is which is on the box, in its own header.
 */
export class ControlBarElement extends LitElement implements Drawable {
  declare store: Store;
  declare control: Control;
  private key = "";

  createRenderRoot() {
    return this;
  }

  private counts() {
    const all = this.control.instruments;
    return {
      n: all.length,
      held: all.filter((n) => this.control.holds(n)).length,
      on: all.filter((n) => armed(this.store, n)).length,
    };
  }

  render() {
    const { n, held, on } = this.counts();
    const some = (k: number) => (k === 0 ? "none" : k === n ? "all" : `${k} of ${n}`);
    return html`<span class="inst ${n > 0 && on === n ? "armed" : ""}">
        <b>${n} instrument${n === 1 ? "" : "s"}</b>
        <span class="who">${n === 0 ? "none on this panel" : `${some(held)} yours · ${some(on)} armed`}</span>
      </span>
      ${n > 0 && held === n
        ? html`<button @click=${() => void this.control.releaseAll()}>Release control</button>`
        : html`<button ?disabled=${n === 0} @click=${() => void this.control.takeAll()}>Take control</button>`}
      ${on > 0
        ? html`<button @click=${() => void this.control.disarmAll()}>Disarm</button>`
        : html`<button ?disabled=${held === 0} @click=${() => void this.control.armAll()}>Arm</button>`}
      <button class="stop" title="Disarm everything and switch every supply output off" @click=${() => this.control.stop()}>
        STOP
      </button>
      ${this.control.message ? html`<span class="ctl-error">${this.control.message}</span>` : ""}`;
  }

  draw() {
    const { n, held, on } = this.counts();
    const key = `${this.control.version}|${this.control.message}|${n}|${held}|${on}`;
    if (key !== this.key) {
      this.key = key;
      this.requestUpdate();
    }
  }
}

/**
 * An instrument box's header: what the box is called, the instrument it is
 * for, and that instrument's own state — its kind, who holds it, whether it is
 * armed. Taking control and arming are the panel's and live in the bar; what
 * is here is what is true of this one instrument.
 *
 * It is a Drawable written from the rack's frame loop rather than a Lit
 * property bound to the store, because it shows live state and §10.1's rule is
 * that the framework never re-renders on data — a header that did would
 * re-render its box, and every widget in it, on every frame.
 */
export class BoxHeadElement extends LitElement implements Drawable {
  static properties = { label: { type: String }, instrument: { type: String } };
  declare label: string;
  declare instrument: string;
  declare store: Store;
  declare control: Control;

  createRenderRoot() {
    return this;
  }

  render() {
    return html`<span class="box-label">${this.label}</span>
      <span class="box-state"></span>
      ${this.instrument ? html`<span class="badge">${this.instrument}</span>` : ""}`;
  }

  draw() {
    const state = this.querySelector(".box-state");
    if (!state) return;
    if (!this.instrument) {
      state.textContent = "";
      return;
    }
    const info = this.store.instruments.get(this.instrument);
    const on = armed(this.store, this.instrument);
    const holder = String(this.store.latest.get(`${this.instrument}.control.holder`) ?? "");
    const who = holder ? (this.control.holds(this.instrument) ? `held by ${holder}, yours` : `held by ${holder}`) : "free";
    state.textContent = [info?.kind, who, on ? "armed" : "not armed"].filter(Boolean).join(" · ");
    this.classList.toggle("armed", on);
    this.classList.toggle("unresolved", !info);
  }
}

// The dials. A gauge shows where a value sits between the ends of its scale
// and a knob sets one, and both are the same drawing: an arc, a needle, and a
// scale that never moves. The arc is drawn once at its full length and shown
// in part by its dash pattern, so a frame costs two attribute writes and no
// path arithmetic.
//
// Degrees are clockwise from straight up, which is how a dial is read.

function polar(cx: number, cy: number, r: number, deg: number): { x: number; y: number } {
  const a = ((deg - 90) * Math.PI) / 180;
  return { x: cx + r * Math.cos(a), y: cy + r * Math.sin(a) };
}

function arcPath(cx: number, cy: number, r: number, from: number, to: number): string {
  const s = polar(cx, cy, r, from);
  const e = polar(cx, cy, r, to);
  const large = Math.abs(to - from) > 180 ? 1 : 0;
  return `M ${s.x.toFixed(2)} ${s.y.toFixed(2)} A ${r} ${r} 0 ${large} 1 ${e.x.toFixed(2)} ${e.y.toFixed(2)}`;
}

/** fraction is where v sits on the scale, 0 to 1, and never off the ends. */
function fraction(v: number, min: number, max: number): number {
  return Math.min(1, Math.max(0, (v - min) / (max - min)));
}

/** tidy drops the floating-point dust a step multiplication leaves behind. */
function tidy(v: number): number {
  return Number(v.toPrecision(12));
}

/**
 * A gauge: one channel on a scale. It answers the question a readout cannot —
 * where this value sits between its ends, and how close it is to one — and it
 * only shows, so it needs neither the lease nor the arming.
 */
export class GaugeElement extends LitElement implements Drawable {
  static properties = {
    channel: { type: String },
    label: { type: String },
    min: { type: Number },
    max: { type: Number },
  };
  declare channel: string;
  declare label: string;
  declare min: number;
  declare max: number;
  declare store: Store;

  /** The arc, drawn whole; draw() shows a part of it with a dash. */
  private static readonly LENGTH = Math.PI * 36;

  createRenderRoot() {
    return this;
  }

  private ends() {
    const min = this.min ?? 0;
    const max = this.max ?? 100;
    return max > min ? { min, max } : { min, max: min + 1 };
  }

  render() {
    const { min, max } = this.ends();
    const arc = arcPath(50, 46, 36, -90, 90);
    const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => {
      const deg = -90 + f * 180;
      const at = polar(50, 46, 46, deg);
      const anchor = deg < -20 ? "end" : deg > 20 ? "start" : "middle";
      return svg`<text class="tick" x=${at.x.toFixed(1)} y=${(at.y + 2).toFixed(1)} text-anchor=${anchor}>${
        format(tidy(min + f * (max - min)))
      }</text>`;
    });
    return html`<div class="title">${this.label || this.channel}</div>
      <svg class="dial" viewBox="-10 0 120 54" preserveAspectRatio="xMidYMid meet">
        <path class="track" d=${arc} />
        <path class="arc" d=${arc} stroke-dasharray="0 999" />
        ${ticks}
        <line class="needle" x1="50" y1="46" x2="50" y2="16" transform="rotate(-90 50 46)" />
        <circle class="hub" cx="50" cy="46" r="2.5" />
      </svg>
      <div class="value">—</div>
      <div class="detail"></div>`;
  }

  draw() {
    const arc = this.querySelector(".arc");
    const needle = this.querySelector(".needle");
    const value = this.querySelector(".value");
    const detail = this.querySelector(".detail");
    if (!arc || !needle || !value || !detail) return;
    const info = this.store.channels.get(this.channel);
    const v = this.store.latest.get(this.channel);
    const { min, max } = this.ends();
    const num = typeof v === "number" ? v : undefined;
    const f = num === undefined ? 0 : fraction(num, min, max);
    const L = GaugeElement.LENGTH;
    arc.setAttribute("stroke-dasharray", `${(f * L).toFixed(2)} ${L.toFixed(2)}`);
    needle.setAttribute("transform", `rotate(${(-90 + f * 180).toFixed(2)} 50 46)`);
    value.textContent = num === undefined ? "—" : `${format(num)} ${info?.unit ?? ""}`;
    // A value off the end of the scale pins the needle, so the widget says so
    // rather than letting a pinned needle read as a steady one.
    const off = num !== undefined && (num < min || num > max);
    detail.textContent = !info ? "no such channel yet" : off ? `off the scale: ${format(num)}` : `${format(min)} to ${format(max)}`;
    this.classList.toggle("unresolved", !info);
    this.classList.toggle("off-scale", off);
  }
}

/**
 * A knob: a scale to turn, which sets a channel. It writes, so it obeys §4
 * exactly as the setpoint does — the needle is what the instrument holds, and
 * what was asked for is a separate mark, shown when the two differ. Dragging
 * it turns it and sending happens on release; the arrow keys step it, so it
 * can be used without a pointer.
 */
export class KnobElement extends LitElement implements Drawable {
  static properties = {
    channel: { type: String },
    label: { type: String },
    step: { type: Number },
    min: { type: Number },
    max: { type: Number },
  };
  declare channel: string;
  declare label: string;
  declare step: number;
  declare min: number;
  declare max: number;
  declare store: Store;
  declare control: Control;

  /** The 270° arc, drawn whole; draw() shows a part of it with a dash. */
  private static readonly LENGTH = 2 * Math.PI * 28 * 0.75;

  /** While a pointer is down: where it is pointing, not yet sent. */
  private turning?: number;
  /** Sent and not yet in the recording's set account. */
  private pending?: number;
  private error = "";

  createRenderRoot() {
    return this;
  }

  private ends() {
    const min = this.min ?? 0;
    const max = this.max ?? 100;
    return max > min ? { min, max } : { min, max: min + 1 };
  }

  /** can says whether this page may write: it holds the lease and the instrument is armed. */
  private can(): boolean {
    const inst = instrumentOf(this.channel);
    return this.control.holds(inst) && armed(this.store, inst);
  }

  render() {
    const { min, max } = this.ends();
    const arc = arcPath(50, 40, 28, -135, 135);
    return html`<div class="title">${this.label || this.channel}</div>
      <svg class="dial" viewBox="-10 0 120 70" preserveAspectRatio="xMidYMid meet">
        <path class="track" d=${arc} />
        <path class="arc" d=${arc} stroke-dasharray="0 999" />
        <text class="tick" x="14" y="66" text-anchor="middle">${format(min)}</text>
        <text class="tick" x="86" y="66" text-anchor="middle">${format(max)}</text>
        <line class="marker" x1="50" y1="4" x2="50" y2="10" transform="rotate(-135 50 40)" />
        <line class="needle" x1="50" y1="40" x2="50" y2="18" transform="rotate(-135 50 40)" />
        <circle class="hub" cx="50" cy="40" r="3" />
      </svg>
      <div class="value">—</div>
      <div class="detail"></div>`;
  }

  firstUpdated() {
    const el = this.querySelector<SVGSVGElement>(".dial");
    if (!el) return;
    el.addEventListener("pointerdown", (e) => {
      if (!this.can()) return;
      el.setPointerCapture(e.pointerId);
      this.turning = this.at(e);
      this.error = "";
      this.draw();
    });
    el.addEventListener("pointermove", (e) => {
      if (this.turning === undefined) return;
      this.turning = this.at(e);
      this.draw();
    });
    el.addEventListener("pointerup", (e) => {
      if (this.turning === undefined) return;
      const v = this.at(e);
      this.turning = undefined;
      void this.send(v);
    });
    el.addEventListener("pointercancel", () => {
      this.turning = undefined;
      this.draw();
    });
    this.addEventListener("keydown", (e) => this.key(e));
  }

  /** at is where the pointer points, on the scale: viewBox coordinates, so the aspect cannot lie. */
  private at(e: PointerEvent): number {
    const el = this.querySelector<SVGSVGElement>(".dial")!;
    const m = el.getScreenCTM();
    const { min, max } = this.ends();
    if (!m) return min;
    const p = new DOMPoint(e.clientX, e.clientY).matrixTransform(m.inverse());
    const deg = (Math.atan2(p.x - 50, 40 - p.y) * 180) / Math.PI;
    const f = (Math.min(135, Math.max(-135, deg)) + 135) / 270;
    const v = min + f * (max - min);
    return this.step > 0 ? tidy(Math.round(v / this.step) * this.step) : tidy(v);
  }

  private key(e: KeyboardEvent) {
    const by = e.key === "ArrowUp" || e.key === "ArrowRight" ? 1 : e.key === "ArrowDown" || e.key === "ArrowLeft" ? -1 : 0;
    if (by === 0 || !this.can()) return;
    e.preventDefault();
    const { min, max } = this.ends();
    const step = this.step > 0 ? this.step : (max - min) / 100;
    const applied = this.store.latest.get(appliedOf(this.channel));
    const from = this.pending ?? (typeof applied === "number" ? applied : min);
    void this.send(tidy(Math.min(max, Math.max(min, from + by * step))));
  }

  private async send(v: number) {
    this.pending = v;
    this.error = "";
    this.draw();
    const a = await this.control.write(this.channel, v);
    if (!a.ok) {
      this.pending = undefined;
      this.error = a.error ?? `HTTP ${a.status}`;
    }
    this.draw();
  }

  draw() {
    const arc = this.querySelector(".arc");
    const needle = this.querySelector(".needle");
    const marker = this.querySelector(".marker");
    const value = this.querySelector(".value");
    const detail = this.querySelector(".detail");
    if (!arc || !needle || !marker || !value || !detail) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const set = this.store.latest.get(this.channel);
    const applied = this.store.latest.get(appliedOf(this.channel));
    if (this.pending !== undefined && set === this.pending) this.pending = undefined;
    const { min, max } = this.ends();
    const unit = info?.unit ?? "";
    const held = typeof applied === "number" ? applied : undefined;
    // The needle is what the instrument holds, except while it is being turned
    // or a value is on its way, when it is what was asked for.
    const shown = this.turning ?? this.pending ?? held;
    const f = shown === undefined ? 0 : fraction(shown, min, max);
    const L = KnobElement.LENGTH;
    arc.setAttribute("stroke-dasharray", `${(f * L).toFixed(2)} ${L.toFixed(2)}`);
    needle.setAttribute("transform", `rotate(${(-135 + f * 270).toFixed(2)} 50 40)`);

    const differs = typeof set === "number" && held !== undefined && Math.abs(set - held) > 1e-9;
    const setF = typeof set === "number" ? fraction(set, min, max) : 0;
    marker.setAttribute("transform", `rotate(${(-135 + setF * 270).toFixed(2)} 50 40)`);
    marker.classList.toggle("shown", differs && this.turning === undefined && this.pending === undefined);

    value.textContent = shown === undefined ? "—" : `${format(shown)} ${unit}`;
    let d = info ? `set ${format(set)} ${set === undefined ? "" : unit}` : "no such channel yet";
    if (this.turning !== undefined) d = `turning to ${format(this.turning)} ${unit}`;
    else if (this.pending !== undefined) d = `sending ${format(this.pending)} ${unit}…`;
    else if (this.error) d = this.error;
    else if (differs) d += ` · the instrument holds ${format(held)} ${unit}`;
    detail.textContent = d;

    const busy = this.turning !== undefined || this.pending !== undefined;
    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", busy);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", differs && !busy);
    this.classList.toggle("locked", !this.can());
    this.tabIndex = this.can() ? 0 : -1;
  }
}

/**
 * A slider: the same thing a knob is, laid out straight. It suits a value
 * with a range worth seeing at a glance — where along its travel this one sits
 * — and it writes on release rather than on every pixel of the drag, so a
 * drag is one decision in the recording and not two hundred.
 */
export class SliderElement extends LitElement implements Drawable {
  static properties = {
    channel: { type: String },
    label: { type: String },
    step: { type: Number },
    min: { type: Number },
    max: { type: Number },
  };
  declare channel: string;
  declare label: string;
  declare step: number;
  declare min: number;
  declare max: number;
  declare store: Store;
  declare control: Control;

  /** While the handle is being dragged: where it is, not yet sent. */
  private moving?: number;
  private pending?: number;
  private error = "";

  createRenderRoot() {
    return this;
  }

  private ends() {
    const min = this.min ?? 0;
    const max = this.max ?? 100;
    return max > min ? { min, max } : { min, max: min + 1 };
  }

  private can(): boolean {
    const inst = instrumentOf(this.channel);
    return this.control.holds(inst) && armed(this.store, inst);
  }

  render() {
    const { min, max } = this.ends();
    return html`<div class="title">${this.label || this.channel}</div>
      <div class="value">—</div>
      <input
        class="range"
        type="range"
        min=${min}
        max=${max}
        step=${this.step > 0 ? String(this.step) : "any"}
        .value=${String(min)}
        @input=${(e: Event) => {
          this.moving = Number((e.target as HTMLInputElement).value);
          this.draw();
        }}
        @change=${(e: Event) => {
          const v = Number((e.target as HTMLInputElement).value);
          this.moving = undefined;
          void this.send(v);
        }}
      />
      <div class="detail"></div>`;
  }

  private async send(v: number) {
    this.pending = v;
    this.error = "";
    this.draw();
    const a = await this.control.write(this.channel, v);
    if (!a.ok) {
      this.pending = undefined;
      this.error = a.error ?? `HTTP ${a.status}`;
    }
    this.draw();
  }

  draw() {
    const value = this.querySelector(".value");
    const detail = this.querySelector(".detail");
    const input = this.querySelector<HTMLInputElement>(".range");
    if (!value || !detail || !input) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const set = this.store.latest.get(this.channel);
    const applied = this.store.latest.get(appliedOf(this.channel));
    if (this.pending !== undefined && set === this.pending) this.pending = undefined;
    const unit = info?.unit ?? "";
    const held = typeof applied === "number" ? applied : undefined;
    const shown = this.moving ?? this.pending ?? held;
    // The handle follows the instrument, except while a hand is on it: moving
    // it under the hand that is dragging it would fight the person using it.
    if (this.moving === undefined && shown !== undefined) input.value = String(shown);

    const differs = typeof set === "number" && held !== undefined && Math.abs(set - held) > 1e-9;
    value.textContent = shown === undefined ? "—" : `${format(shown)} ${unit}`;
    let d = info ? `set ${format(set)} ${set === undefined ? "" : unit}` : "no such channel yet";
    if (this.moving !== undefined) d = `moving to ${format(this.moving)} ${unit}`;
    else if (this.pending !== undefined) d = `sending ${format(this.pending)} ${unit}…`;
    else if (this.error) d = this.error;
    else if (differs) d += ` · the instrument holds ${format(held)} ${unit}`;
    detail.textContent = d;

    const busy = this.moving !== undefined || this.pending !== undefined;
    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", busy);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", differs && !busy);
    input.disabled = !this.can();
  }
}

/**
 * A momentary button: on while it is held, off when it is let go. It is the
 * widget for something that must not stay on by itself — a hold-to-run, an
 * output that should follow a finger.
 *
 * So the release matters more than the press, and every way of letting go is
 * one: the pointer up, the pointer leaving, the pointer cancelled, the key
 * released, the button losing focus, the page being hidden. A press that
 * cannot be released is exactly the failure this widget must not have. It is
 * still not an interlock — the daemon's arming and the stop are (§7) — because
 * a browser that dies holds nothing, and a bench must not depend on one.
 */
export class MomentaryElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String } };
  declare channel: string;
  declare label: string;
  declare store: Store;
  declare control: Control;

  /** What this widget last asked for, so that a release is sent once. */
  private held = false;
  private busy = false;
  private error = "";

  createRenderRoot() {
    return this;
  }

  private can(): boolean {
    const inst = instrumentOf(this.channel);
    return this.control.holds(inst) && armed(this.store, inst);
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <button class="press">HOLD</button>
      <div class="detail"></div>`;
  }

  firstUpdated() {
    const b = this.querySelector<HTMLButtonElement>(".press");
    if (!b) return;
    b.addEventListener("pointerdown", (e) => {
      b.setPointerCapture(e.pointerId);
      void this.set(true);
    });
    for (const ev of ["pointerup", "pointercancel", "pointerleave", "blur"] as const) {
      b.addEventListener(ev, () => void this.set(false));
    }
    b.addEventListener("keydown", (e) => {
      if ((e.key === " " || e.key === "Enter") && !e.repeat) void this.set(true);
    });
    b.addEventListener("keyup", (e) => {
      if (e.key === " " || e.key === "Enter") void this.set(false);
    });
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) void this.set(false);
    });
  }

  /** set asks for on or off once. A release is sent even when a press would not be allowed. */
  private async set(on: boolean) {
    if (on && !this.can()) return;
    if (this.held === on) return;
    this.held = on;
    this.busy = true;
    this.error = "";
    this.draw();
    const a = await this.control.write(this.channel, on);
    this.busy = false;
    if (!a.ok) {
      this.error = a.error ?? `HTTP ${a.status}`;
      if (on) this.held = false;
    }
    this.draw();
  }

  draw() {
    const button = this.querySelector<HTMLButtonElement>(".press");
    const detail = this.querySelector(".detail");
    if (!button || !detail) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const applied = this.store.latest.get(appliedOf(this.channel));
    const on = applied === true;
    button.classList.toggle("on", on);
    let d = info ? (on ? "the instrument reports on" : "off") : "no such channel yet";
    if (this.busy) d = this.held ? "pressing…" : "releasing…";
    else if (this.error) d = this.error;
    else if (this.held !== on) d = this.held ? "held, waiting for the instrument" : `let go, and the instrument reports ${on ? "on" : "off"}`;
    detail.textContent = d;
    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", this.busy);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", !this.busy && this.held !== on);
    button.disabled = !this.can() && !this.held;
  }
}

/**
 * An enum selector: a few values worth naming, and nothing else offered. A
 * timebase of 1 ms, a range, a mode. The instrument may hold something that is
 * not on the list — it was set from elsewhere, or clamped — and the list does
 * not hide that: the selector shows no choice, and says what is held.
 */
export class EnumElement extends LitElement implements Drawable {
  static properties = { channel: { type: String }, label: { type: String }, options: { type: Array } };
  declare channel: string;
  declare label: string;
  declare options: { value: number; label?: string }[];
  declare store: Store;
  declare control: Control;

  private pending?: number;
  private error = "";

  createRenderRoot() {
    return this;
  }

  private can(): boolean {
    const inst = instrumentOf(this.channel);
    return this.control.holds(inst) && armed(this.store, inst);
  }

  render() {
    return html`<div class="title">${this.label || this.channel}</div>
      <select
        class="choice"
        @change=${(e: Event) => {
          const v = (e.target as HTMLSelectElement).value;
          if (v !== "") void this.send(Number(v));
        }}
      >
        <option value="">—</option>
        ${(this.options ?? []).map((o) => html`<option value=${String(o.value)}>${o.label || format(o.value)}</option>`)}
      </select>
      <div class="detail"></div>`;
  }

  private async send(v: number) {
    this.pending = v;
    this.error = "";
    this.draw();
    const a = await this.control.write(this.channel, v);
    if (!a.ok) {
      this.pending = undefined;
      this.error = a.error ?? `HTTP ${a.status}`;
    }
    this.draw();
  }

  draw() {
    const select = this.querySelector<HTMLSelectElement>(".choice");
    const detail = this.querySelector(".detail");
    if (!select || !detail) return;
    const info = this.store.channels.get(appliedOf(this.channel));
    const set = this.store.latest.get(this.channel);
    const applied = this.store.latest.get(appliedOf(this.channel));
    if (this.pending !== undefined && set === this.pending) this.pending = undefined;
    const unit = info?.unit ?? "";
    const held = typeof applied === "number" ? applied : undefined;
    const shown = this.pending ?? held;
    // A value the list does not offer is shown as no choice at all, never as
    // the nearest one: a selector that rounded would be saying the instrument
    // holds something it does not.
    const match = shown === undefined ? undefined : (this.options ?? []).find((o) => Math.abs(o.value - shown) < 1e-9);
    select.value = match ? String(match.value) : "";
    const differs = typeof set === "number" && held !== undefined && Math.abs(set - held) > 1e-9;
    let d = info ? `set ${format(set)} ${set === undefined ? "" : unit}` : "no such channel yet";
    if (this.pending !== undefined) d = `sending ${format(this.pending)} ${unit}…`;
    else if (this.error) d = this.error;
    else if (differs) d += ` · the instrument holds ${format(held)} ${unit}`;
    else if (shown !== undefined && !match) d = `the instrument holds ${format(shown)} ${unit}, which is not on the list`;
    detail.textContent = d;
    this.classList.toggle("unresolved", !info);
    this.classList.toggle("pending", this.pending !== undefined);
    this.classList.toggle("refused", this.error !== "");
    this.classList.toggle("differs", differs && this.pending === undefined);
    select.disabled = !this.can();
  }
}

customElements.define("ktest-readout", ReadoutElement);
customElements.define("ktest-plot", PlotElement);
customElements.define("ktest-scope", ScopeElement);
customElements.define("ktest-channels", ChannelsElement);
customElements.define("ktest-setpoint", SetpointElement);
customElements.define("ktest-toggle", ToggleElement);
customElements.define("ktest-lamp", LampElement);
customElements.define("ktest-control", ControlBarElement);
customElements.define("ktest-box-head", BoxHeadElement);
customElements.define("ktest-gauge", GaugeElement);
customElements.define("ktest-knob", KnobElement);
customElements.define("ktest-slider", SliderElement);
customElements.define("ktest-button", MomentaryElement);
customElements.define("ktest-enum", EnumElement);
