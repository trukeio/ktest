// A panel is data (§10.1): widgets on a grid, bound to channels by name. This
// is its shape as ktestd serves and checks it (cmd/ktestd/panel.go), and the
// file a save writes back.
//
// A panel is either one grid of widgets or a stack of boxes, one to an
// instrument, each its own grid. Widget coordinates belong to the grid they
// are in, so a box can be moved without rewriting what is under it.

export type WidgetType =
  | "readout"
  | "plot"
  | "scope"
  | "setpoint"
  | "toggle"
  | "lamp"
  | "gauge"
  | "knob"
  | "slider"
  | "button"
  | "enum";

/** One choice an enum offers: the value it sets, and what it is called. */
export interface Option {
  value: number;
  label?: string;
}

export interface WidgetSpec {
  type: WidgetType;
  label?: string;
  channel?: string;
  channels?: string[];
  window?: number;
  step?: number;
  /** gauge, knob and slider: the ends of the scale. Both unsaid means 0 to 100. */
  min?: number;
  max?: number;
  /** enum: the values it offers. */
  options?: Option[];
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A group is one instrument's box: its own grid, of the panel's width unless it says otherwise. */
export interface Group {
  instrument?: string;
  label?: string;
  columns?: number;
  widgets: WidgetSpec[];
}

export interface Panel {
  title?: string;
  columns?: number;
  /** One grid, the older shape; a panel has this or groups, never both. */
  widgets?: WidgetSpec[];
  groups?: Group[];
}

/** Which widget is picked: the box it is in, and its place in that box. */
export interface Pick {
  g: number;
  i: number;
}

export const TYPES: WidgetType[] = [
  "readout",
  "gauge",
  "plot",
  "scope",
  "setpoint",
  "knob",
  "slider",
  "enum",
  "toggle",
  "button",
  "lamp",
];

/** The size a new widget starts at, in grid cells. */
export const SIZE: Record<WidgetType, { w: number; h: number }> = {
  readout: { w: 3, h: 2 },
  plot: { w: 6, h: 4 },
  scope: { w: 6, h: 4 },
  setpoint: { w: 3, h: 2 },
  toggle: { w: 2, h: 2 },
  lamp: { w: 2, h: 1 },
  gauge: { w: 3, h: 3 },
  knob: { w: 2, h: 3 },
  slider: { w: 4, h: 2 },
  button: { w: 2, h: 2 },
  enum: { w: 3, h: 2 },
};

/** The ends of a scale, as it is drawn: both unsaid means 0 to 100. */
export function scaleOf(w: { min?: number; max?: number }): { min: number; max: number } {
  const min = w.min ?? 0;
  const max = w.max ?? 0;
  if (min === 0 && max === 0) return { min: 0, max: 100 };
  return max > min ? { min, max } : { min, max: min + 1 };
}

export function columnsOf(p: Panel): number {
  return p.columns || 12;
}

/**
 * groupsOf is the boxes a panel has, which is the panel itself when it is one
 * grid. The widgets are the panel's own objects, not copies: what the rack
 * writes back into them when one is dragged reaches the panel.
 */
export function groupsOf(p?: Panel): Group[] {
  if (!p) return [];
  if (p.groups) return p.groups;
  return [{ widgets: p.widgets ?? [] }];
}

/** columnsOfGroup is a box's width: its own, or the panel's. */
export function columnsOfGroup(p: Panel, g: Group): number {
  return g.columns || columnsOf(p);
}

/** normalize is the panel as the editor holds it: always boxes, never a bare grid. */
export function normalize(p: Panel): Panel {
  const out: Panel = { groups: groupsOf(p).map((g) => ({ ...g, widgets: [...g.widgets] })) };
  if (p.title) out.title = p.title;
  if (p.columns) out.columns = p.columns;
  return out;
}

/** writes says whether a widget sets its channel rather than showing it. */
export function writes(t: WidgetType): boolean {
  return t === "setpoint" || t === "toggle" || t === "knob" || t === "slider" || t === "button" || t === "enum";
}

/** switches says whether a widget works a channel that is on or off rather than a number. */
export function switches(t: WidgetType): boolean {
  return t === "toggle" || t === "button";
}

/** scaled says whether a widget draws its channel between two ends, and so has them. */
export function scaled(t: WidgetType): boolean {
  return t === "gauge" || t === "knob" || t === "slider";
}

/** stepped says whether a widget's value moves in steps a person picks. */
export function stepped(t: WidgetType): boolean {
  return t === "setpoint" || t === "knob" || t === "slider";
}

// A channel that can be set, as ktestd's parseChannel names one: the
// instrument, the part of it, the quantity, and set. The part is whatever lies
// between, because the instruments do not all divide the same way — a supply's
// is a channel, a scope's is ch1.vert or timebase.
const SETTABLE = /^([^.]+)\.(.+)\.([^.]+)\.set$/;

/** A quantity that is a switch rather than a number, and so a toggle's or a button's. */
const SWITCHES = ["out", "on"];

/** A scope channel, which is what a scope widget draws: scope1.ch1. */
const WAVEFORM = /^[^.]+\.ch[1-9][0-9]*$/;

/** instrumentOf is the instrument a channel belongs to: the name before the first dot. */
export function instrumentOf(channel: string): string {
  return channel.split(".")[0];
}

/** accepts says whether a widget of type t may be bound to channel c. */
export function accepts(t: WidgetType, c: string): boolean {
  if (c === "") return false;
  const m = SETTABLE.exec(c);
  if (writes(t)) return m !== null && SWITCHES.includes(m[3]) === switches(t);
  if (t === "scope") return WAVEFORM.test(c);
  return true;
}

/**
 * instrumentsOf is the instruments a panel is for, each leased and armed on
 * its own. A box says which instrument it is for; in a panel that is one grid
 * there is nothing to say it, so the instruments written to are taken instead
 * — which is why a box is worth naming: an instrument shown only through
 * readouts has a lease too.
 */
export function instrumentsOf(p?: Panel): string[] {
  const names: string[] = [];
  for (const g of groupsOf(p)) {
    if (g.instrument) names.push(g.instrument);
    for (const w of g.widgets) if (writes(w.type)) names.push(instrumentOf(w.channel ?? ""));
  }
  return [...new Set(names.filter((n) => n !== ""))].sort();
}

/** plottedOf is every channel a panel's plots draw, whose every sample the page keeps. */
export function plottedOf(p?: Panel): string[] {
  return groupsOf(p).flatMap((g) => g.widgets.flatMap((w) => (w.type === "plot" ? (w.channels ?? []) : [])));
}

/** A problem is something ktestd would refuse, and where it is; -1 for the panel's own. */
export interface Problem {
  group: number;
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
  if (!Number.isInteger(cols) || cols < 1 || cols > 48) {
    out.push({ group: -1, widget: -1, text: "a panel has 1 to 48 columns" });
  }
  groupsOf(p).forEach((g, gi) => {
    const gcols = columnsOfGroup(p, g);
    if (!Number.isInteger(gcols) || gcols < 1 || gcols > 48) {
      out.push({ group: gi, widget: -1, text: "a box has 1 to 48 columns" });
    }
    g.widgets.forEach((w, i) => {
      const say = (text: string) => out.push({ group: gi, widget: i, text });
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
        case "knob":
        case "slider":
          if (!accepts(w.type, w.channel ?? "")) {
            say(`a ${w.type} sets a channel such as psu1.ch1.v.set or scope1.timebase.tdiv.set`);
          }
          if ((w.step ?? 0) < 0) say("a step cannot be negative");
          break;
        case "enum":
          if (!accepts(w.type, w.channel ?? "")) say("an enum sets a channel such as psu1.ch1.v.set");
          if ((w.options ?? []).length < 2) say("an enum offers at least two options");
          else {
            const seen = new Set<number>();
            for (const o of w.options ?? []) {
              if (!Number.isFinite(o.value)) say("an option sets a number");
              else if (seen.has(o.value)) say(`two options set ${o.value}`);
              seen.add(o.value);
            }
          }
          break;
        case "toggle":
        case "button":
          if (!accepts(w.type, w.channel ?? "")) say(`a ${w.type} switches something, such as psu1.ch1.out.set`);
          break;
        default:
          if (!w.channel) say(`a ${w.type} needs a channel`);
      }
      // The scale, held exactly as the daemon holds it: both ends unsaid is
      // the 0 to 100 a scale falls back to, and anything else must be an
      // interval, since a needle on a scale of no width says nothing.
      if ((w.min ?? 0) !== 0 || (w.max ?? 0) !== 0) {
        if (!scaled(w.type)) say("only a gauge, a knob or a slider has a scale");
        else if (!((w.max ?? 0) > (w.min ?? 0))) say(`a scale from ${w.min ?? 0} to ${w.max ?? 0}`);
      }
      if ((w.options ?? []).length > 0 && w.type !== "enum") say("only an enum has options");
      if ((w.step ?? 0) !== 0 && !stepped(w.type)) say("only a setpoint, a knob or a slider has a step");
      // A box that names an instrument and holds another's widget is a
      // contradiction inside the file, which the daemon refuses too.
      if (g.instrument) {
        for (const c of [w.channel ?? "", ...(w.channels ?? [])]) {
          if (c && instrumentOf(c) !== g.instrument) say(`${c} is not ${g.instrument}'s`);
        }
      }
      if (w.x + w.w > gcols) say(`it runs past the ${gcols} columns of its box`);
      for (let j = 0; j < i; j++) {
        const o = g.widgets[j];
        if (w.x < o.x + o.w && o.x < w.x + w.w && w.y < o.y + o.h && o.y < w.y + w.h) say(`it overlaps widget ${j + 1}`);
      }
    });
  });
  return out;
}

/**
 * format writes a panel as a file: one widget to a line, in reading order,
 * with only what differs from the defaults — the text a person would write,
 * which is what the daemon keeps, the -panel file becomes and the recording
 * embeds. A panel of one box that is for nothing in particular is written as a
 * bare grid, so that a simple panel stays a simple file.
 */
export function format(p: Panel): string {
  const parts: string[] = [];
  if (p.title) parts.push(`  "title": ${JSON.stringify(p.title)}`);
  if (p.columns && p.columns !== 12) parts.push(`  "columns": ${p.columns}`);
  const gs = groupsOf(p);
  if (gs.length === 1 && !gs[0].instrument && !gs[0].label && !gs[0].columns) {
    parts.push(widgetList(gs[0].widgets, 2));
  } else {
    const boxes = gs.map((g) => {
      const inner: string[] = [];
      if (g.instrument) inner.push(`      "instrument": ${JSON.stringify(g.instrument)}`);
      if (g.label) inner.push(`      "label": ${JSON.stringify(g.label)}`);
      if (g.columns) inner.push(`      "columns": ${g.columns}`);
      inner.push(widgetList(g.widgets, 6));
      return `    {\n${inner.join(",\n")}\n    }`;
    });
    parts.push(boxes.length ? `  "groups": [\n${boxes.join(",\n")}\n  ]` : `  "groups": []`);
  }
  return `{\n${parts.join(",\n")}\n}\n`;
}

function widgetList(ws: WidgetSpec[], pad: number): string {
  const p = " ".repeat(pad);
  const lines = [...ws].sort((a, b) => a.y - b.y || a.x - b.x).map((w) => `${p}  ${line(w)}`);
  return lines.length ? `${p}"widgets": [\n${lines.join(",\n")}\n${p}]` : `${p}"widgets": []`;
}

function line(w: WidgetSpec): string {
  const o: [string, unknown][] = [["type", w.type]];
  if (w.type === "plot") o.push(["channels", w.channels ?? []]);
  else o.push(["channel", w.channel ?? ""]);
  if (w.label) o.push(["label", w.label]);
  if (w.type === "plot" && w.window) o.push(["window", w.window]);
  if (w.type === "enum") o.push(["options", w.options ?? []]);
  if (stepped(w.type) && w.step) o.push(["step", w.step]);
  if (scaled(w.type) && w.min) o.push(["min", w.min]);
  if (scaled(w.type) && w.max) o.push(["max", w.max]);
  o.push(["x", w.x], ["y", w.y], ["w", w.w], ["h", w.h]);
  const value = (v: unknown) => (Array.isArray(v) ? `[${v.map((x) => JSON.stringify(x)).join(", ")}]` : JSON.stringify(v));
  return `{${o.map(([k, v]) => `"${k}": ${value(v)}`).join(", ")}}`;
}
