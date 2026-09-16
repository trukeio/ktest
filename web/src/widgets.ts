// The widgets. Each owns its rendering surface: Lit draws its frame once, and
// data reaches it only through draw(), which writes into the surface directly
// (§10.1). The rack calls draw() from its one animation frame loop.

import { LitElement, html } from "lit";
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
 * The control bar: each instrument the panel writes to — who holds it and
 * whether it is armed, from its control stream — with the buttons to take it,
 * arm it and let it go, and the stop, which anyone may press at any time.
 */
export class ControlBarElement extends LitElement implements Drawable {
  declare store: Store;
  declare control: Control;
  private key = "";

  createRenderRoot() {
    return this;
  }

  private state(name: string) {
    const l = this.store.latest;
    return {
      armed: l.get(`${name}.control.armed`) === true,
      holder: String(l.get(`${name}.control.holder`) ?? ""),
      mine: this.control.holds(name),
    };
  }

  render() {
    return html`${this.control.instruments.map((n) => {
        const s = this.state(n);
        return html`<span class="inst ${s.armed ? "armed" : ""}">
          <b>${n}</b>
          <span class="who">${s.holder ? `held by ${s.holder}` : "free"}${s.armed ? " · armed" : ""}</span>
          ${s.mine
            ? html`<button @click=${() => this.control.release(n)}>Release</button>
                ${s.armed ? "" : html`<button @click=${() => this.control.arm(n)}>Arm</button>`}`
            : html`<button @click=${() => this.control.take(n)}>Take control</button>`}
          ${s.armed ? html`<button @click=${() => this.control.disarm(n)}>Disarm</button>` : ""}
        </span>`;
      })}
      <button class="stop" title="Disarm everything and switch every supply output off" @click=${() => this.control.stop()}>
        STOP
      </button>
      ${this.control.message ? html`<span class="ctl-error">${this.control.message}</span>` : ""}`;
  }

  draw() {
    const key = `${this.control.version}|${this.control.message}|` +
      this.control.instruments.map((n) => JSON.stringify(this.state(n))).join("|");
    if (key !== this.key) {
      this.key = key;
      this.requestUpdate();
    }
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
