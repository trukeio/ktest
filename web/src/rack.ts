// The rack's surface: a panel laid out as a stack of instrument boxes, each a
// gridstack grid of its own (§10.2). The same grids show the rack and edit it,
// so what is edited is exactly what will be seen, drawing live data all the
// while.
//
// gridstack owns each grid and where its items are; the widgets inside own
// their surfaces (§10.1). Nothing here re-renders on data: build() lays a
// panel out once, and the rack's frame loop calls each widget's draw().

import { GridStack } from "gridstack";
import type { Store } from "./store.ts";
import type { Control } from "./control.ts";
import type { Drawable } from "./widgets.ts";
import {
  columnsOfGroup,
  groupsOf,
  scaleOf,
  scaled,
  stepped,
  type Group,
  type Panel,
  type Pick,
  type WidgetSpec,
} from "./panel.ts";

const TAGS = {
  readout: "ktest-readout",
  plot: "ktest-plot",
  scope: "ktest-scope",
  setpoint: "ktest-setpoint",
  toggle: "ktest-toggle",
  lamp: "ktest-lamp",
  gauge: "ktest-gauge",
  knob: "ktest-knob",
  slider: "ktest-slider",
  button: "ktest-button",
  enum: "ktest-enum",
} as const;

/** One grid row, in pixels: 64 for the widget and 8 between rows, as the rack has always been. */
const ROW = 72;

export class Rack {
  /** Every widget on the rack, boxes in order: what the frame loop draws. */
  widgets: Drawable[] = [];
  /** editing lets widgets be dragged, resized, picked and removed; build() applies it. */
  editing = false;
  onSelect: (pick: Pick) => void = () => {};
  onRemove: (pick: Pick) => void = () => {};
  /** onMove is told after a widget has been dragged or resized, once the panel says where it is. */
  onMove: () => void = () => {};

  private host: HTMLElement;
  private store: Store;
  private control: Control;
  private grids: GridStack[] = [];

  constructor(host: HTMLElement, store: Store, control: Control) {
    this.host = host;
    this.store = store;
    this.control = control;
  }

  /**
   * build lays p out afresh, one box to a group. Widgets are cheap to make:
   * the samples they draw are in the store, not in them. While editing, a drag
   * or resize is written into p itself, so the panel is always where the grids
   * show it.
   */
  build(p: Panel, selected: Pick | null = null) {
    this.clear();
    const groups = groupsOf(p);
    groups.forEach((g, gi) => {
      const box = document.createElement("section");
      box.className = "instrument";
      box.dataset.group = String(gi);
      const head = this.head(g, gi);
      if (head) box.append(head);
      const el = document.createElement("div");
      el.className = "grid-stack";
      box.append(el);
      this.host.append(box);
      g.widgets.forEach((w, i) => {
        const item = document.createElement("div");
        item.className = "grid-stack-item";
        item.classList.toggle("selected", selected?.g === gi && selected?.i === i);
        item.dataset.group = String(gi);
        item.dataset.index = String(i);
        for (const k of ["x", "y", "w", "h"] as const) item.setAttribute(`gs-${k}`, String(w[k]));
        const content = document.createElement("div");
        content.className = "grid-stack-item-content";
        const widget = this.make(w);
        const remove = document.createElement("button");
        remove.className = "remove";
        remove.title = "Remove this widget";
        remove.textContent = "✕";
        remove.addEventListener("click", (e) => {
          e.stopPropagation();
          this.onRemove({ g: gi, i });
        });
        content.append(widget, remove);
        content.addEventListener("click", () => {
          if (this.editing) this.onSelect({ g: gi, i });
        });
        item.append(content);
        el.append(item);
        this.widgets.push(widget);
      });
      const grid = GridStack.init(
        {
          column: Math.min(48, Math.max(1, columnsOfGroup(p, g))),
          cellHeight: ROW,
          margin: 4,
          // Widgets stay where the panel puts them, rather than rising to fill
          // any space above: the layout is the panel's, not gridstack's.
          float: true,
          animate: false,
          staticGrid: !this.editing,
          minRow: this.editing ? 4 : 1,
        },
        el,
      )!;
      grid.on("change", (_e, nodes) => {
        for (const n of nodes) {
          const w = groups[Number(n.el?.dataset.group)]?.widgets[Number(n.el?.dataset.index)];
          if (!w) continue;
          w.x = n.x ?? w.x;
          w.y = n.y ?? w.y;
          w.w = n.w ?? w.w;
          w.h = n.h ?? w.h;
        }
        this.onMove();
      });
      this.grids.push(grid);
    });
  }

  /** clear takes the rack down: no grids, no boxes, no widgets. */
  clear() {
    for (const g of this.grids) g.destroy(true);
    this.grids = [];
    this.host.replaceChildren();
    this.widgets = [];
  }

  /** select marks a widget as the one being edited; null for none. */
  select(pick: Pick | null) {
    for (const item of this.host.querySelectorAll<HTMLElement>(".grid-stack-item")) {
      const mine = pick !== null && item.dataset.group === String(pick.g) && item.dataset.index === String(pick.i);
      item.classList.toggle("selected", mine);
    }
  }

  // head is a box's strip: what it is called, the instrument it is for as a
  // badge, and that instrument's state, drawn from the frame loop like any
  // other live thing. A box that is for nothing and says nothing has no strip
  // at all, except while editing, where it is what there is to click.
  private head(g: Group, gi: number): HTMLElement | null {
    if (!g.label && !g.instrument && !this.editing) return null;
    const head = document.createElement("ktest-box-head") as unknown as Drawable & Record<string, unknown>;
    head.className = "box-head";
    Object.assign(head, {
      store: this.store,
      control: this.control,
      label: g.label || g.instrument || `Box ${gi + 1}`,
      instrument: g.instrument ?? "",
    });
    this.widgets.push(head);
    return head as unknown as HTMLElement;
  }

  private make(w: WidgetSpec): Drawable {
    const el = document.createElement(TAGS[w.type]) as unknown as Drawable & Record<string, unknown>;
    el.classList.add("widget");
    Object.assign(el, { store: this.store, control: this.control, label: w.label ?? "" });
    if (w.type === "plot") Object.assign(el, { channels: w.channels ?? [], window: w.window ?? 10 });
    else Object.assign(el, { channel: w.channel ?? "" });
    if (stepped(w.type)) Object.assign(el, { step: w.step ?? 0 });
    // The scale is resolved here rather than in the widget, so that the file's
    // silence means one thing in one place.
    if (scaled(w.type)) Object.assign(el, scaleOf(w));
    if (w.type === "enum") Object.assign(el, { options: w.options ?? [] });
    return el;
  }
}
