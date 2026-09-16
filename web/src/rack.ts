// The rack's surface: a panel laid out on a gridstack grid (§10.2). The same
// grid shows the rack and edits it, so what is edited is exactly what will be
// seen, drawing live data all the while.
//
// gridstack owns the grid and where its items are; the widgets inside own
// their surfaces (§10.1). Nothing here re-renders on data: build() lays a
// panel out once, and the rack's frame loop calls each widget's draw().

import { GridStack } from "gridstack";
import type { Store } from "./store.ts";
import type { Control } from "./control.ts";
import type { Drawable } from "./widgets.ts";
import { columnsOf, type Panel, type WidgetSpec } from "./panel.ts";

const TAGS = {
  readout: "ktest-readout",
  plot: "ktest-plot",
  scope: "ktest-scope",
  setpoint: "ktest-setpoint",
  toggle: "ktest-toggle",
  lamp: "ktest-lamp",
} as const;

/** One grid row, in pixels: 64 for the widget and 8 between rows, as the rack has always been. */
const ROW = 72;

export class Rack {
  widgets: Drawable[] = [];
  /** editing lets widgets be dragged, resized, picked and removed; build() applies it. */
  editing = false;
  onSelect: (i: number) => void = () => {};
  onRemove: (i: number) => void = () => {};
  /** onMove is told after a widget has been dragged or resized, once the panel says where it is. */
  onMove: () => void = () => {};

  private host: HTMLElement;
  private store: Store;
  private control: Control;
  private grid?: GridStack;

  constructor(host: HTMLElement, store: Store, control: Control) {
    this.host = host;
    this.store = store;
    this.control = control;
  }

  /**
   * build lays p out afresh. Widgets are cheap to make: the samples they draw
   * are in the store, not in them. While editing, a drag or resize is written
   * into p itself, so the panel is always where the grid shows it.
   */
  build(p: Panel, selected = -1) {
    this.clear();
    const el = document.createElement("div");
    el.className = "grid-stack";
    this.host.append(el);
    p.widgets.forEach((w, i) => {
      const item = document.createElement("div");
      item.className = "grid-stack-item";
      item.classList.toggle("selected", i === selected);
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
        this.onRemove(i);
      });
      content.append(widget, remove);
      content.addEventListener("click", () => {
        if (this.editing) this.onSelect(i);
      });
      item.append(content);
      el.append(item);
      this.widgets.push(widget);
    });
    this.grid = GridStack.init(
      {
        column: Math.min(48, Math.max(1, columnsOf(p))),
        cellHeight: ROW,
        margin: 4,
        // Widgets stay where the panel puts them, rather than rising to fill
        // any space above: the layout is the panel's, not gridstack's.
        float: true,
        animate: false,
        staticGrid: !this.editing,
        minRow: this.editing ? 8 : 1,
      },
      el,
    )!;
    this.grid.on("change", (_e, nodes) => {
      for (const n of nodes) {
        const w = p.widgets[Number(n.el?.dataset.index)];
        if (!w) continue;
        w.x = n.x ?? w.x;
        w.y = n.y ?? w.y;
        w.w = n.w ?? w.w;
        w.h = n.h ?? w.h;
      }
      this.onMove();
    });
  }

  /** clear takes the rack down: no grid, no widgets. */
  clear() {
    this.grid?.destroy(true);
    this.grid = undefined;
    this.host.replaceChildren();
    this.widgets = [];
  }

  /** select marks widget i as the one being edited; -1 for none. */
  select(i: number) {
    for (const item of this.host.querySelectorAll<HTMLElement>(".grid-stack-item")) {
      item.classList.toggle("selected", item.dataset.index === String(i));
    }
  }

  private make(w: WidgetSpec): Drawable {
    const el = document.createElement(TAGS[w.type]) as unknown as Drawable & Record<string, unknown>;
    el.classList.add("widget");
    Object.assign(el, { store: this.store, control: this.control, label: w.label ?? "" });
    if (w.type === "plot") Object.assign(el, { channels: w.channels ?? [], window: w.window ?? 10 });
    else Object.assign(el, { channel: w.channel ?? "" });
    if (w.type === "setpoint") Object.assign(el, { step: w.step ?? 0 });
    return el;
  }
}
