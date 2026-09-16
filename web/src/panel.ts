// A panel is data (§10.1): widgets on a grid, bound to channels by name. This
// is its shape as ktestd serves and checks it (cmd/ktestd/panel.go), and the
// file a save writes back.

export type WidgetType = "readout" | "plot" | "scope" | "setpoint" | "toggle" | "lamp";

export interface WidgetSpec {
  type: WidgetType;
  label?: string;
  channel?: string;
  channels?: string[];
  window?: number;
  step?: number;
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface Panel {
  title?: string;
  columns?: number;
  widgets: WidgetSpec[];
}

export const TYPES: WidgetType[] = ["readout", "plot", "scope", "setpoint", "toggle", "lamp"];

/** The size a new widget starts at, in grid cells. */
export const SIZE: Record<WidgetType, { w: number; h: number }> = {
  readout: { w: 3, h: 2 },
  plot: { w: 6, h: 4 },
  scope: { w: 6, h: 4 },
  setpoint: { w: 3, h: 2 },
  toggle: { w: 2, h: 2 },
  lamp: { w: 2, h: 1 },
};

export function columnsOf(p: Panel): number {
  return p.columns || 12;
}

/** writes says whether a widget sets its channel rather than showing it. */
export function writes(t: WidgetType): boolean {
  return t === "setpoint" || t === "toggle";
}

// A channel that can be set, as ktestd's parseChannel names one: the
// instrument, the part of it, the quantity, and set. The part is whatever lies
// between, because the instruments do not all divide the same way — a supply's
// is a channel, a scope's is ch1.vert or timebase.
const SETTABLE = /^([^.]+)\.(.+)\.([^.]+)\.set$/;

/** A quantity that is a switch rather than a number, and so a toggle's. */
const SWITCHES = ["out", "on"];

/** A scope channel, which is what a scope widget draws: scope1.ch1. */
const WAVEFORM = /^[^.]+\.ch[1-9][0-9]*$/;

/** accepts says whether a widget of type t may be bound to channel c. */
export function accepts(t: WidgetType, c: string): boolean {
  if (c === "") return false;
  const m = SETTABLE.exec(c);
  if (t === "setpoint") return m !== null && !SWITCHES.includes(m[3]);
  if (t === "toggle") return m !== null && SWITCHES.includes(m[3]);
  if (t === "scope") return WAVEFORM.test(c);
  return true;
}

/** instrumentsOf is the instruments a panel writes to, each leased and armed on its own. */
export function instrumentsOf(p?: Panel): string[] {
  const names = (p?.widgets ?? []).filter((w) => writes(w.type)).map((w) => (w.channel ?? "").split(".")[0]);
  return [...new Set(names)].sort();
}

/** plottedOf is every channel a panel's plots draw, whose every sample the page keeps. */
export function plottedOf(p?: Panel): string[] {
  return (p?.widgets ?? []).flatMap((w) => (w.type === "plot" ? (w.channels ?? []) : []));
}

/** A problem is something ktestd would refuse, and the widget it is in; -1 for the panel's own. */
export interface Problem {
  widget: number;
  text: string;
}

/**
 * problems is what ktestd would refuse in a panel, said beside the widget
 * before anything is sent. The daemon checks again on save, and its word is
 * the one that counts; this only spares a round trip.
 */
export function problems(p: Panel): Problem[] {
  const out: Problem[] = [];
  const cols = columnsOf(p);
  if (!Number.isInteger(cols) || cols < 1 || cols > 48) out.push({ widget: -1, text: "a panel has 1 to 48 columns" });
  p.widgets.forEach((w, i) => {
    const say = (text: string) => out.push({ widget: i, text });
    switch (w.type) {
      case "plot":
        if (!w.channels?.length) say("a plot needs a channel to draw");
        else if (w.channels.includes("")) say("one of its channels is empty");
        if ((w.window ?? 0) < 0) say("a window cannot be negative");
        break;
      case "scope":
        if (!accepts(w.type, w.channel ?? "")) say("a scope draws a scope channel, such as scope1.ch1");
        break;
      case "setpoint":
        if (!accepts(w.type, w.channel ?? "")) {
          say("a setpoint sets a channel such as psu1.ch1.v.set or scope1.timebase.tdiv.set");
        }
        if ((w.step ?? 0) < 0) say("a step cannot be negative");
        break;
      case "toggle":
        if (!accepts(w.type, w.channel ?? "")) say("a toggle switches something, such as psu1.ch1.out.set");
        break;
      default:
        if (!w.channel) say(`a ${w.type} needs a channel`);
    }
    if (w.x + w.w > cols) say(`it runs past the grid's ${cols} columns`);
    for (let j = 0; j < i; j++) {
      const o = p.widgets[j];
      if (w.x < o.x + o.w && o.x < w.x + w.w && w.y < o.y + o.h && o.y < w.y + w.h) say(`it overlaps widget ${j + 1}`);
    }
  });
  return out;
}

/**
 * format writes a panel as a file: one widget to a line, in reading order,
 * with only what differs from the defaults — the text a person would write,
 * which is what the daemon keeps, the -panel file becomes and the recording
 * embeds.
 */
export function format(p: Panel): string {
  const parts: string[] = [];
  if (p.title) parts.push(`  "title": ${JSON.stringify(p.title)}`);
  if (p.columns && p.columns !== 12) parts.push(`  "columns": ${p.columns}`);
  const ws = [...p.widgets].sort((a, b) => a.y - b.y || a.x - b.x).map((w) => `    ${line(w)}`);
  parts.push(ws.length ? `  "widgets": [\n${ws.join(",\n")}\n  ]` : `  "widgets": []`);
  return `{\n${parts.join(",\n")}\n}\n`;
}

function line(w: WidgetSpec): string {
  const o: [string, unknown][] = [["type", w.type]];
  if (w.type === "plot") o.push(["channels", w.channels ?? []]);
  else o.push(["channel", w.channel ?? ""]);
  if (w.label) o.push(["label", w.label]);
  if (w.type === "plot" && w.window) o.push(["window", w.window]);
  if (w.type === "setpoint" && w.step) o.push(["step", w.step]);
  o.push(["x", w.x], ["y", w.y], ["w", w.w], ["h", w.h]);
  const value = (v: unknown) => (Array.isArray(v) ? `[${v.map((x) => JSON.stringify(x)).join(", ")}]` : JSON.stringify(v));
  return `{${o.map(([k, v]) => `"${k}": ${value(v)}`).join(", ")}}`;
}
