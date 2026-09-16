// The panel editor: the panel's own settings, widgets to add, and the picked
// widget's bindings. It edits a draft, and the rack is rebuilt from the draft
// on every change, so what is on screen is what a save would send. Nothing
// reaches the daemon until then.

import { LitElement, html, nothing } from "lit";
import type { Store } from "./store.ts";
import { SIZE, TYPES, accepts, columnsOf, problems, type Panel, type WidgetSpec, type WidgetType } from "./panel.ts";

/** What the editor asks of the page that holds it. */
export interface EditorHost {
  /** changed says the draft changed, and which widget is picked now if that did too. */
  changed(select?: number): void;
  select(i: number): void;
  removeWidget(i: number): void;
  save(): void;
  discard(): void;
}

const val = (e: Event) => (e.target as HTMLInputElement).value.trim();

/** What a widget's channel looks like, as a hint in the empty field. */
const PLACEHOLDER: Partial<Record<WidgetType, string>> = {
  setpoint: "psu1.ch1.v.set",
  toggle: "psu1.ch1.out.set",
  scope: "scope1.ch1",
};

export class EditorElement extends LitElement {
  static properties = {
    selected: { type: Number },
    message: { type: String },
    warn: { type: Boolean },
    saving: { type: Boolean },
  };
  declare draft: Panel;
  declare store: Store;
  declare app: EditorHost;
  declare selected: number;
  /** message is what the last save came to; warn marks it as bad news. */
  declare message: string;
  declare warn: boolean;
  declare saving: boolean;

  constructor() {
    super();
    this.selected = -1;
    this.message = "";
    this.warn = false;
    this.saving = false;
  }

  createRenderRoot() {
    return this;
  }

  render() {
    const d = this.draft;
    const found = problems(d);
    const sel = d.widgets[this.selected];
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
      <div class="add">
        <span>Add</span>
        ${TYPES.map((t) => html`<button @click=${() => this.add(t)}>${t}</button>`)}
      </div>
      ${sel
        ? this.widget(sel, this.selected)
        : html`<p class="hint">Click a widget to bind it. Drag it to move it, and its lower right corner to resize it.</p>`}
      ${found.length
        ? html`<ul class="problems">
            ${found.map(
              (p) =>
                html`<li>
                  ${p.widget < 0
                    ? nothing
                    : html`<a
                          href="#"
                          @click=${(e: Event) => {
                            e.preventDefault();
                            this.app.select(p.widget);
                          }}
                          >widget ${p.widget + 1}</a
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

  // widget is the picked widget's form: what it is bound to, and how it shows it.
  private widget(w: WidgetSpec, i: number) {
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
      <legend>Widget ${i + 1}, a ${w.type}</legend>
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
      ${w.type === "setpoint"
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
      <button class="danger" @click=${() => this.app.removeWidget(i)}>Remove this widget</button>
    </fieldset>`;
  }

  // add puts a new widget below everything else, bound to the first channel
  // it could show, and picks it.
  private add(t: WidgetType) {
    const d = this.draft;
    const y = Math.max(0, ...d.widgets.map((w) => w.y + w.h));
    const first = [...this.store.channels.keys()].sort().find((c) => accepts(t, c)) ?? "";
    const w: WidgetSpec = { type: t, x: 0, y, w: Math.min(SIZE[t].w, columnsOf(d)), h: SIZE[t].h };
    if (t === "plot") w.channels = first ? [first] : [];
    else w.channel = first;
    d.widgets.push(w);
    this.app.changed(d.widgets.length - 1);
  }
}

customElements.define("ktest-editor", EditorElement);
