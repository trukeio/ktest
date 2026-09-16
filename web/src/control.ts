// The rack's control side: leases, arming, writes and the stop (§7).
//
// Nothing here decides what is true. Whether an instrument is armed, and who
// holds it, is read from its control stream like any other channel; a
// request's answer says only whether it was taken (§6). The page keeps a
// lease's token in memory and nowhere else: a reloaded page takes the lease
// again, and the daemon hands the same one back, since over TLS a lease
// belongs to the name that took it.

/** A request's answer: taken or not, its number, and why not. */
export interface Answer {
  ok: boolean;
  status: number;
  seq?: number;
  error?: string;
  token?: string;
}

// A lease's term is short, so that a page that went away stops holding an
// instrument soon after; it is renewed well inside it.
const TTL = "15s";
const RENEW_MS = 5000;

export class Control {
  /** The instruments the control bar lists: those the panel writes to, and any this page holds. */
  instruments: string[];
  /** version changes whenever what the page holds does. */
  version = 0;
  /** message is the last thing that went wrong, "" when nothing has. */
  message = "";

  private auth: string;
  private tokens = new Map<string, string>();
  private timers = new Map<string, number>();
  /**
   * What kind each instrument is, which decides the path its lease is taken on:
   * /psu/{name}/lease, /scope/{name}/lease. It is read out of the instrument's
   * own control stream and set by the page; until the stream has said, a supply
   * is the assumption, because it is the only kind that existed when these
   * paths were named.
   */
  private kinds = new Map<string, string>();

  constructor(auth: string, instruments: string[]) {
    this.auth = auth;
    this.instruments = instruments;
  }

  /** knows tells the control side what kind each instrument is. */
  knows(kinds: Map<string, string>) {
    for (const [name, kind] of kinds) this.kinds.set(name, kind);
  }

  /** path is an instrument's endpoint: the kind, its name, and what to do. */
  private path(name: string, what: string): string {
    return `${this.kinds.get(name) ?? "psu"}/${name}/${what}`;
  }

  /**
   * show lists the instruments a panel writes to. One this page holds stays
   * listed whatever the panel says, so that its lease can still be let go.
   */
  show(names: string[]) {
    const next = [...new Set([...names, ...this.tokens.keys()])].sort();
    if (next.join("\n") === this.instruments.join("\n")) return;
    this.instruments = next;
    this.version++;
  }

  /**
   * savePanel asks for text to be the panel in force. base is the sha256 of
   * the panel it was edited from, so that a save over someone else's is
   * refused rather than made. The panel arrives on the stream once it is.
   */
  savePanel(text: string, base: string): Promise<Answer> {
    return this.request("PUT", "panel", { "Content-Type": "application/json", "If-Match": `"${base}"` }, text);
  }

  /** holds says whether this page holds the instrument's lease. */
  holds(name: string): boolean {
    return this.tokens.has(name);
  }

  async take(name: string) {
    const a = await this.call("POST", this.path(name, "lease"), { ttl: TTL });
    if (!a.ok || !a.token) return this.fail(name, a);
    this.tokens.set(name, a.token);
    if (!this.timers.has(name)) this.timers.set(name, window.setInterval(() => void this.renew(name), RENEW_MS));
    this.changed("");
  }

  async release(name: string) {
    const a = await this.call("DELETE", this.path(name, "lease"), undefined, name);
    this.drop(name);
    if (a.ok) this.changed("");
    else this.fail(name, a);
  }

  async arm(name: string) {
    const a = await this.call("POST", this.path(name, "arm"), undefined, name);
    if (!a.ok) this.fail(name, a);
  }

  /**
   * disarm stops the instrument taking writes, and switches a supply's outputs
   * off. It needs no lease: stopping is always safe, and a stop that needed the
   * lease would be out of reach exactly when the client holding it has hung.
   */
  async disarm(name: string) {
    const a = await this.call("POST", this.path(name, "disarm"), undefined, name);
    if (!a.ok) this.fail(name, a);
  }

  /** stop disarms everything the daemon drives: the bus, the supplies, the scopes. */
  async stop() {
    const a = await this.call("POST", "disarm");
    if (a.ok) this.changed("");
    else this.fail("stop", a);
  }

  /** write sets a channel such as psu1.ch1.v.set, under its instrument's lease. */
  write(channel: string, value: number | boolean): Promise<Answer> {
    return this.call("PUT", `channels/${encodeURIComponent(channel)}`, { value }, channel.split(".")[0]);
  }

  private async renew(name: string) {
    const a = await this.call("PUT", this.path(name, "lease"), { ttl: TTL }, name);
    if (a.ok) return;
    // Refused means the lease is gone — expired, or released elsewhere — and
    // the page must stop acting as if it held it. A request that never reached
    // the daemon says nothing about the lease, and the next renewal may.
    if (a.status !== 0) {
      this.drop(name);
      this.fail(name, a, "the lease was lost");
    }
  }

  private drop(name: string) {
    const t = this.timers.get(name);
    if (t !== undefined) window.clearInterval(t);
    this.timers.delete(name);
    this.tokens.delete(name);
    this.version++;
  }

  private fail(name: string, a: Answer, what?: string) {
    this.changed(`${name}: ${what ? `${what}: ` : ""}${a.error ?? `HTTP ${a.status}`}`);
  }

  private changed(message: string) {
    this.message = message;
    this.version++;
  }

  private call(method: string, path: string, body?: unknown, lease?: string): Promise<Answer> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const token = lease === undefined ? undefined : this.tokens.get(lease);
    if (token !== undefined) headers["Ktest-Lease"] = token;
    return this.request(method, path, headers, body === undefined ? undefined : JSON.stringify(body));
  }

  private async request(method: string, path: string, headers: Record<string, string>, body?: string): Promise<Answer> {
    headers.Authorization = `Bearer ${this.auth}`;
    try {
      const res = await fetch(path, { method, headers, body, cache: "no-store" });
      let j: { seq?: number; error?: string; token?: string } = {};
      try {
        j = await res.json();
      } catch {
        // An answer that is not JSON still has a status.
      }
      return { ok: res.ok, status: res.status, seq: j.seq, error: j.error, token: j.token };
    } catch (e) {
      return { ok: false, status: 0, error: e instanceof Error ? e.message : String(e) };
    }
  }
}
