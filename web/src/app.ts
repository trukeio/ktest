// The rack: log in, fetch the panel, lay it out, draw — and edit it.
//
// The panel in force is the daemon's: the one GET /panel answers with, and
// after that whichever the stream last carried. A save is a request like any
// other (§6): its answer says only whether it was taken, and the page leaves
// editing when the recording carries the panel it sent.

import { LitElement, html, nothing } from "lit";
import { Store } from "./store.ts";
import { Control } from "./control.ts";
import { Rack } from "./rack.ts";
import { format, instrumentsOf, plottedOf, type Panel } from "./panel.ts";
import type { Drawable } from "./widgets.ts";
import type { EditorElement, EditorHost } from "./editor.ts";
import "./widgets.ts";
import "./editor.ts";

const TOKEN = "ktest.token";

/** A panel as the daemon holds it: its text, that text's sha256, and what it says. */
interface Held {
  text: string;
  sha: string;
  panel: Panel;
}

export class AppElement extends LitElement implements EditorHost {
  static properties = { state: { state: true }, error: { state: true }, editing: { state: true } };
  declare state: "login" | "loading" | "rack";
  declare error: string;
  declare editing: boolean;

  private store?: Store;
  private control?: Control;
  private rack?: Rack;
  /** The panel in force; undefined while the daemon has none. */
  private held?: Held;
  /** While editing: the draft, the sha256 of the panel it began from, and the widget picked. */
  private draft?: Panel;
  private base = "";
  private selected = -1;
  /** A save sent and not yet carried by the stream: the text sent. */
  private saving?: string;
  /** A panel someone else saved while this page was editing. */
  private newer?: Held;
  private panelSeen = 0;
  private channelsSeen = -1;
  private others: Drawable[] = [];
  private drawn = -1;

  constructor() {
    super();
    this.state = "loading";
    this.error = "";
    this.editing = false;
  }

  createRenderRoot() {
    return this;
  }

  connectedCallback() {
    super.connectedCallback();
    const token = sessionStorage.getItem(TOKEN);
    if (token === null) this.state = "login";
    else void this.open(token);
  }

  private async open(token: string) {
    this.state = "loading";
    let res: Response;
    try {
      res = await fetch("panel", { headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
    } catch (e) {
      this.error = `The daemon cannot be reached: ${e instanceof Error ? e.message : e}`;
      this.state = "login";
      return;
    }
    if (res.status === 401) {
      sessionStorage.removeItem(TOKEN);
      this.error = "That token is not one this daemon admits.";
      this.state = "login";
      return;
    }
    // No panel is not an error: the rack shows every channel instead.
    if (res.ok) {
      const text = await res.text();
      this.held = { text, sha: (res.headers.get("ETag") ?? "").replaceAll('"', ""), panel: JSON.parse(text) as Panel };
    }
    sessionStorage.setItem(TOKEN, token);
    this.store = new Store(token);
    this.control = new Control(token, instrumentsOf(this.held?.panel));
    this.state = "rack";
    await this.updateComplete;

    this.rack = new Rack(this.querySelector<HTMLElement>(".grid-host")!, this.store, this.control);
    this.rack.onSelect = (i) => this.select(i);
    this.rack.onRemove = (i) => this.removeWidget(i);
    this.rack.onMove = () => this.editor()?.requestUpdate();
    this.layout();
    requestAnimationFrame(this.frame);
  }

  updated() {
    this.others = [...this.querySelectorAll<Drawable>("ktest-control, ktest-channels")];
    this.drawn = -1;
  }

  // layout builds the rack from what is on show: the draft while editing,
  // otherwise the panel in force.
  private layout() {
    const p = this.draft ?? this.held?.panel;
    const rack = this.rack!;
    rack.editing = this.draft !== undefined;
    if (p) {
      rack.build(p, this.selected);
      this.store!.subscribe(plottedOf(p));
    } else rack.clear();
    this.control!.show(instrumentsOf(p));
    this.drawn = -1;
  }

  // The one animation frame loop for the whole rack (§10.3): whatever arrived
  // since the last frame is drawn once, and every widget lands on the same frame.
  private frame = () => {
    const s = this.store!;
    if (s.panelVersion !== this.panelSeen) {
      this.panelSeen = s.panelVersion;
      this.arrived(s.panel!, s.panelSha);
    }
    if (s.version !== this.drawn) {
      this.drawn = s.version;
      for (const w of this.rack!.widgets) w.draw();
      for (const w of this.others) w.draw();
      if (s.channelsVersion !== this.channelsSeen) {
        // The editor offers the channels the stream carries, and the control
        // side learns what kind each instrument is: a supply and a scope are
        // leased on paths of their own (§7), and the stream is where the
        // recording says which is which.
        this.channelsSeen = s.channelsVersion;
        this.control!.knows(new Map([...s.instruments].map(([n, i]) => [n, i.kind])));
        this.editor()?.requestUpdate();
      }
      const st = this.querySelector(".status");
      if (st) {
        st.textContent = `${s.status.state}${s.status.detail ? `: ${s.status.detail}` : ""} · ` +
          `${(s.status.bytes / 1e6).toFixed(1)} MB · ${s.channels.size} channels` +
          (s.status.refused ? ` · ${s.status.refused} frames refused` : "");
        st.className = `status ${s.status.state}`;
      }
    }
    requestAnimationFrame(this.frame);
  };

  // arrived takes a panel the stream carried: the daemon's word on what is in force.
  private arrived(text: string, sha: string) {
    let panel: Panel;
    try {
      panel = JSON.parse(text) as Panel;
    } catch {
      return; // the daemon checks every panel it keeps; this one is not the rack's to show
    }
    const h = { text, sha, panel };
    if (this.saving !== undefined && text === this.saving) {
      // This page's own save, now in the recording: the edit is done.
      this.held = h;
      this.stopEditing();
      return;
    }
    if (text === this.held?.text) {
      this.held = h;
      return;
    }
    if (this.draft) {
      this.newer = h;
      this.say("Someone has saved another panel since this edit began, so saving this one will be refused. " +
        "Discard to load theirs.", true);
      return;
    }
    this.held = h;
    this.requestUpdate();
    this.layout();
  }

  private edit() {
    this.draft = structuredClone(this.held?.panel ?? { widgets: [] });
    this.base = this.held?.sha ?? "";
    this.selected = -1;
    this.newer = undefined;
    this.editing = true;
    this.layout();
  }

  private stopEditing() {
    this.draft = this.saving = this.newer = undefined;
    this.selected = -1;
    this.editing = false;
    this.layout();
  }

  // EditorHost: what the editor, and the rack while editing, ask of the page.

  changed(select?: number) {
    if (select !== undefined) this.selected = select;
    this.layout();
    this.requestUpdate();
    const ed = this.editor();
    if (ed) {
      ed.selected = this.selected;
      ed.requestUpdate();
    }
  }

  select(i: number) {
    this.selected = i;
    this.rack?.select(i);
    const ed = this.editor();
    if (ed) ed.selected = i;
  }

  removeWidget(i: number) {
    this.draft?.widgets.splice(i, 1);
    this.changed(-1);
  }

  async save() {
    const text = format(this.draft!);
    this.saving = text;
    this.say("Saving…", false, true);
    const a = await this.control!.savePanel(text, this.base);
    if (this.saving !== text) return; // the stream carried it before the answer came
    if (!a.ok) {
      this.saving = undefined;
      const why = a.error ?? `HTTP ${a.status}`;
      this.say(a.status === 412 ? `${why}. Discard to load the panel now in force.` : why, true);
      return;
    }
    this.say(`Taken as request ${a.seq}; waiting for the recording to carry it…`, false, true);
  }

  discard() {
    if (this.newer) this.held = this.newer;
    this.stopEditing();
  }

  private say(message: string, warn: boolean, saving = false) {
    const ed = this.editor();
    if (!ed) return;
    ed.message = message;
    ed.warn = warn;
    ed.saving = saving;
  }

  private editor(): EditorElement | null {
    return this.querySelector<EditorElement>("ktest-editor");
  }

  private submit(e: Event) {
    e.preventDefault();
    const input = this.querySelector<HTMLInputElement>("input[name=token]");
    const token = input?.value.trim() ?? "";
    if (token) void this.open(token);
  }

  private logout() {
    sessionStorage.removeItem(TOKEN);
    location.reload();
  }

  render() {
    switch (this.state) {
      case "login":
        return html`<form class="login" @submit=${this.submit}>
          <h1>ktest rack</h1>
          <p>This daemon admits named tokens. Paste yours; it is kept for this tab only.</p>
          <input name="token" type="password" autocomplete="off" placeholder="token" autofocus />
          <button type="submit">Open the rack</button>
          ${this.error ? html`<p class="error">${this.error}</p>` : ""}
        </form>`;
      case "loading":
        return html`<p class="loading">Opening the rack…</p>`;
    }
    const p = this.draft ?? this.held?.panel;
    return html`<header>
        <span class="name">${p?.title || "ktest rack"}</span>
        <span class="status connecting">connecting</span>
        ${this.editing
          ? html`<span class="mode">editing</span>`
          : html`<button @click=${this.edit}>Edit panel</button>`}
        <button @click=${this.logout}>Log out</button>
      </header>
      <ktest-control .store=${this.store} .control=${this.control}></ktest-control>
      <main class="workspace ${this.editing ? "editing" : ""}">
        <div class="grid-host" ?hidden=${!p}></div>
        ${p ? nothing : html`<div class="table"><ktest-channels .store=${this.store}></ktest-channels></div>`}
        ${this.editing
          ? html`<ktest-editor .draft=${this.draft} .store=${this.store} .app=${this} .selected=${this.selected}></ktest-editor>`
          : nothing}
      </main>`;
  }
}

customElements.define("ktest-app", AppElement);
