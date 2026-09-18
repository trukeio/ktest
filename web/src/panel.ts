// A panel is data (§10.1): widgets on a grid, bound to channels by name. This
// is its shape as ktestd serves and checks it (cmd/ktestd/panel.go), and the
// file a save writes back.
//
// A panel is either one grid of widgets or a stack of boxes, one to an
// instrument, each its own grid. Widget coordinates belong to the grid they
// are in, so a box can be moved without rewriting what is under it.

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

/** instrumentOf is the instrument a channel belongs to: the name before the first dot. */
export function instrumentOf(channel: string): string {
  return channel.split(".")[0];
}

/** accepts says whether a widget of type t may be bound to channel c. */
export function accepts(t: WidgetType, c: string): boolean {
  if (c === "") return false;
  const m = SETTABLE.exec(c);
  if (t === "setpoint") return m !== null && !SWITCHES.includes(m[3]);
  if (t === "toggle") return m !== null && SWITCHES.includes(m[3]);
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
  if (w.type === "setpoint" && w.step) o.push(["step", w.step]);
  o.push(["x", w.x], ["y", w.y], ["w", w.w], ["h", w.h]);
  const value = (v: unknown) => (Array.isArray(v) ? `[${v.map((x) => JSON.stringify(x)).join(", ")}]` : JSON.stringify(v));
  return `{${o.map(([k, v]) => `"${k}": ${value(v)}`).join(", ")}}`;
}
