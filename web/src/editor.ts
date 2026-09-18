// The panel editor: the panel's own settings, the boxes it stacks, widgets to
// add, and the picked widget's bindings. It edits a draft, and the rack is
// rebuilt from the draft on every change, so what is on screen is what a save
// would send. Nothing reaches the daemon until then.

import { LitElement, html, nothing } from "lit";
import type { Store } from "./store.ts";
import {
  SIZE,
  TYPES,
  accepts,
  columnsOf,
  columnsOfGroup,
  dial,
  groupsOf,
  problems,
  scaleOf,
  stepped,
  type Group,
  type Panel,
  type Pick,
  type WidgetSpec,
  type WidgetType,
} from "./panel.ts";

/** What the editor asks of the page that holds it. */
export interface EditorHost {
  /** changed says the draft changed, and which widget is picked now if that did too. */
  changed(select?: Pick | null): void;
  select(pick: Pick | null): void;
  removeWidget(pick: Pick): void;
  save(): void;
  discard(): void;
}

const val = (e: Event) => (e.target as HTMLInputElement).value.trim();

/** What a widget's channel looks like, as a hint in the empty field. */
const PLACEHOLDER: Partial<Record<WidgetType, string>> = {
  setpoint: "psu1.ch1.v.set",
  knob: "psu1.ch1.v.set",
  toggle: "psu1.ch1.out.set",
  scope: "scope1.ch1",
  gauge: "psu1.ch1.meas.v",
};

export class EditorElement extends LitElement {
  static properties = {
    selected: { type: Object },
    group: { type: Number },
    message: { type: String },
    warn: { type: Boolean },
    saving: { type: Boolean },
  };
  declare draft: Panel;
  declare store: Store;
  declare app: EditorHost;
  declare selected: Pick | null;
  /** The box a new widget goes into: the picked widget's, or the one last picked. */
  declare group: number;
  /** message is what the last save came to; warn marks it as bad news. */
  declare message: string;
  declare warn: boolean;
  declare saving: boolean;

  constructor() {
    super();
    this.selected = null;
    this.group = 0;
    this.message = "";
    this.warn = false;
    this.saving = false;
  }

  createRenderRoot() {
    return this;
  }

  /** target is the box being added to. */
  private get target(): number {
    return this.selected ? this.selected.g : this.group;
  }

  render() {
    const d = this.draft;
    const groups = groupsOf(d);
    const found = problems(d);
    const sel = this.selected ? groups[this.selected.g]?.widgets[this.selected.i] : undefined;
    const rev = this.store.latest.get("rack.panel.revision");
    const by = this.store.latest.get("rack.panel.saved_by");
    return html`<h2>Editing the panel</h2>
      <p class="dim">
        ${rev === undefined || rev === 0 ? "No panel is in force yet." : `From revision ${rev}${by ? `, saved by ${by}` : ""}.`}
      </p>
      <label
        >Title
        <input
          .value=${d.title ?? ""}
          @change=${(e: Event) => {
            d.title = val(e) || undefined;
            this.app.changed();
          }}
      /></label>
      <label
        >Columns
        <input
          type="number"
          min="1"
          max="48"
          step="1"
          .value=${String(columnsOf(d))}
          @change=${(e: Event) => {
            const n = Number(val(e));
            d.columns = n === 12 ? undefined : n;
            this.app.changed();
          }}
      /></label>
      ${this.boxes(groups)} ${this.box(groups[this.target], this.target)}
      <div class="add">
        <span>Add to ${this.name(groups[this.target], this.target)}</span>
        ${TYPES.map((t) => html`<button @click=${() => this.add(t)}>${t}</button>`)}
      </div>
      ${sel
        ? this.widget(sel, this.selected!)
        : html`<p class="hint">Click a widget to bind it. Drag it to move it, and its lower right corner to resize it.</p>`}
      ${found.length
        ? html`<ul class="problems">
            ${found.map(
              (p) =>
                html`<li>
                  ${p.widget < 0
                    ? p.group < 0
                      ? nothing
                      : html`${this.name(groups[p.group], p.group)}: `
                    : html`<a
                          href="#"
                          @click=${(e: Event) => {
                            e.preventDefault();
                            this.app.select({ g: p.group, i: p.widget });
                          }}
                          >${this.name(groups[p.group], p.group)}, widget ${p.widget + 1}</a
                        >: `}${p.text}
                </li>`,
            )}
          </ul>`
        : nothing}
      ${this.message ? html`<p class="msg ${this.warn ? "bad" : ""}">${this.message}</p>` : nothing}
      <div class="actions">
        <button class="primary" ?disabled=${found.length > 0 || this.saving} @click=${() => this.app.save()}>
          ${this.saving ? "Saving…" : "Save"}
        </button>
        <button @click=${() => this.app.discard()}>Discard</button>
      </div>`;
  }

  // name is what a box is called in the editor's own text.
  private name(g: Group | undefined, gi: number): string {
    return g?.instrument || g?.label || `box ${gi + 1}`;
  }

  // boxes is the stack itself: which boxes there are, in the order they are
  // drawn, and the ways to reorder them. A box moves by swapping places in the
  // list rather than by being dragged, since what a box's position means is
  // its place in the stack and nothing else.
  private boxes(groups: Group[]) {
    return html`<div class="boxes">
      <span class="dim">Boxes, top to bottom</span>
      ${groups.map(
        (g, gi) => html`<div class="row box ${gi === this.target ? "picked" : ""}">
          <button
            class="pick"
            @click=${() => {
              this.group = gi;
              this.app.select(null);
            }}
          >
            ${this.name(g, gi)}
          </button>
          <button title="Move this box up" ?disabled=${gi === 0} @click=${() => this.move(gi, -1)}>↑</button>
          <button title="Move this box down" ?disabled=${gi === groups.length - 1} @click=${() => this.move(gi, 1)}>
            ↓
          </button>
          <button title="Remove this box and its widgets" @click=${() => this.removeGroup(gi)}>✕</button>
        </div>`,
      )}
      <button @click=${() => this.addGroup()}>Add a box</button>
    </div>`;
  }

  // box is the picked box's own form: the instrument it is for, what it is
  // called, and how wide its grid is.
  private box(g: Group | undefined, gi: number) {
    if (!g) return nothing;
    const d = this.draft;
    // Every instrument the stream has named, so that a box is bound to one
    // that exists; a name not in the list is still allowed, since a panel is
    // written before its streams arrive.
    const list = "ktest-instruments";
    return html`<fieldset>
      <legend>${this.name(g, gi)}</legend>
      <datalist id=${list}>${[...this.store.instruments.keys()].sort().map((n) => html`<option value=${n}></option>`)}</datalist>
      <label
        >Instrument
        <input
          list=${list}
          .value=${g.instrument ?? ""}
          placeholder="psu1"
          @change=${(e: Event) => {
            g.instrument = val(e) || undefined;
            this.app.changed();
          }}
      /></label>
      ${g.instrument && !this.store.instruments.has(g.instrument)
        ? html`<p class="warn">${g.instrument} is not in the stream yet</p>`
        : nothing}
      <label
        >Label
        <input
          .value=${g.label ?? ""}
          placeholder="the instrument's name"
          @change=${(e: Event) => {
            g.label = val(e) || undefined;
            this.app.changed();
          }}
      /></label>
      <label
        >Columns
        <input
          type="number"
          min="1"
          max="48"
          step="1"
          .value=${String(columnsOfGroup(d, g))}
          @change=${(e: Event) => {
            const n = Number(val(e));
            g.columns = n === columnsOf(d) ? undefined : n;
            this.app.changed();
          }}
      /></label>
    </fieldset>`;
  }

  // widget is the picked widget's form: what it is bound to, and how it shows it.
  private widget(w: WidgetSpec, pick: Pick) {
    // A waveform holds no value, so only a scope is offered one; everything
    // else would be bound to a channel it could never read.
    const options = [...this.store.channels.values()]
      .filter((c) => accepts(w.type, c.name) && (c.kind === "waveform") === (w.type === "scope"))
      .map((c) => c.name)
      .sort();
    const list = `ktest-channels-${w.type}`;
    const unknown = (c: string) =>
      c && !this.store.channels.has(c) ? html`<p class="warn">${c} is not in the stream yet</p>` : nothing;
    return html`<fieldset>
      <legend>Widget ${pick.i + 1}, a ${w.type}</legend>
      <datalist id=${list}>${options.map((c) => html`<option value=${c}></option>`)}</datalist>
      <label
        >Label
        <input
          .value=${w.label ?? ""}
          placeholder="the channel's name"
          @change=${(e: Event) => {
            w.label = val(e) || undefined;
            this.app.changed();
          }}
      /></label>
      ${w.type === "plot"
        ? html`<label>Channels</label>
            ${(w.channels ?? []).map(
              (c, k) =>
                html`<div class="row">
                    <input
                      list=${list}
                      .value=${c}
                      @change=${(e: Event) => {
                        w.channels![k] = val(e);
                        this.app.changed();
                      }}
                    />
                    <button
                      title="Stop drawing this channel"
                      @click=${() => {
                        w.channels!.splice(k, 1);
                        this.app.changed();
                      }}
                    >
                      ✕
                    </button>
                  </div>
                  ${unknown(c)}`,
            )}
            <input
              list=${list}
              placeholder="add a channel"
              @change=${(e: Event) => {
                const c = val(e);
                if (!c) return;
                (w.channels ??= []).push(c);
                (e.target as HTMLInputElement).value = "";
                this.app.changed();
              }}
            />
            <label
              >Window, seconds
              <input
                type="number"
                min="0"
                step="any"
                .value=${String(w.window ?? 10)}
                @change=${(e: Event) => {
                  w.window = Number(val(e)) || undefined;
                  this.app.changed();
                }}
            /></label>`
        : html`<label
              >Channel
              <input
                list=${list}
                .value=${w.channel ?? ""}
                placeholder=${PLACEHOLDER[w.type] ?? ""}
                @change=${(e: Event) => {
                  w.channel = val(e);
                  this.app.changed();
                }}
            /></label>
            ${unknown(w.channel ?? "")}`}
      ${stepped(w.type)
        ? html`<label
            >Step
            <input
              type="number"
              min="0"
              step="any"
              .value=${String(w.step ?? 0)}
              @change=${(e: Event) => {
                w.step = Number(val(e)) || undefined;
                this.app.changed();
              }}
          /></label>`
        : nothing}
      ${dial(w.type)
        ? html`<div class="row">
            <label
              >Scale from
              <input
                type="number"
                step="any"
                .value=${String(scaleOf(w).min)}
                @change=${(e: Event) => {
                  w.min = Number(val(e)) || undefined;
                  this.app.changed();
                }}
            /></label>
            <label
              >to
              <input
                type="number"
                step="any"
                .value=${String(scaleOf(w).max)}
                @change=${(e: Event) => {
                  w.max = Number(val(e)) || undefined;
                  this.app.changed();
                }}
            /></label>
          </div>`
        : nothing}
      <button class="danger" @click=${() => this.app.removeWidget(pick)}>Remove this widget</button>
    </fieldset>`;
  }

  // add puts a new widget at the bottom of the box being added to, bound to
  // the first channel it could show, and picks it. A box for an instrument
  // offers that instrument's channels first, since that is what it is for.
  private add(t: WidgetType) {
    const groups = groupsOf(this.draft);
    const gi = this.target;
    const g = groups[gi];
    if (!g) return;
    const y = Math.max(0, ...g.widgets.map((w) => w.y + w.h));
    const names = [...this.store.channels.keys()].sort();
    const mine = g.instrument ? names.filter((c) => c.startsWith(`${g.instrument}.`)) : names;
    const first = [...mine, ...names].find((c) => accepts(t, c)) ?? "";
    const w: WidgetSpec = { type: t, x: 0, y, w: Math.min(SIZE[t].w, columnsOfGroup(this.draft, g)), h: SIZE[t].h };
    if (t === "plot") w.channels = first ? [first] : [];
    else w.channel = first;
    g.widgets.push(w);
    this.app.changed({ g: gi, i: g.widgets.length - 1 });
  }

  private addGroup() {
    const groups = this.draft.groups!;
    groups.push({ widgets: [] });
    this.group = groups.length - 1;
    this.app.changed(null);
  }

  private removeGroup(gi: number) {
    this.draft.groups!.splice(gi, 1);
    if (this.group >= gi) this.group = Math.max(0, this.group - 1);
    this.app.changed(null);
  }

  private move(gi: number, by: number) {
    const groups = this.draft.groups!;
    const to = gi + by;
    if (to < 0 || to >= groups.length) return;
    [groups[gi], groups[to]] = [groups[to], groups[gi]];
    this.group = to;
    this.app.changed(null);
  }
}

customElements.define("ktest-editor", EditorElement);
