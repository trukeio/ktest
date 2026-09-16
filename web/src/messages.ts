// What the page and the stream worker say to each other.

/** A channel is a stream's name and a field's (§4, §10.1). */
export interface ChannelInfo {
  name: string;
  stream: string;
  field: string;
  unit: string;
  /**
   * A waveform is a channel nothing reads a value from: it is a scope channel,
   * whose acquisitions arrive whole and are drawn by a scope widget. It is
   * listed so that a panel can be bound to it before the first trigger, and so
   * that a readout offered it would be offering something that holds no value.
   */
  kind: "number" | "text" | "waveform";
  held: boolean;
}

/**
 * An instrument as the rack knows it: its name and what kind it is, which is
 * the path its lease is taken on — /psu/{name}/lease, /scope/{name}/lease. The
 * kind comes from the instrument's own control stream rather than from the
 * shape of its channel names, because a guess there would break the first time
 * a third kind arrived.
 */
export interface InstrumentInfo {
  name: string;
  kind: string;
}

export type Latest = number | string | boolean;

export type ToWorker =
  | { type: "start"; url: string; token: string }
  | { type: "subscribe"; channels: string[] };

export interface Status {
  state: "connecting" | "live" | "ended" | "error";
  detail: string;
  bytes: number;
  refused: number;
}

/**
 * An Acquisition is one scope trigger, reduced to what a screen can draw: a
 * min/max envelope, one bucket a pixel column (§10.4), with the settings it was
 * taken under beside it.
 *
 * It replaces its predecessor rather than extending it, which is what makes it
 * a different message from "data": a waveform is not a growing series, it is a
 * picture taken again. The arrays are transferred, not copied.
 */
export interface Acquisition {
  /** The envelope stream, and the channel it reduces: scope1.ch1.env, scope1.ch1. */
  stream: string;
  of: string;
  run: number;

  /** Seconds from the trigger for each bucket's centre, so the trigger is at 0. */
  t: Float64Array;
  min: Float64Array;
  max: Float64Array;
  /** Samples in each bucket. Zero is a gap, not a zero volt. */
  n: Int32Array;

  /** Where the acquisition sits on the recording's timeline, and what it was taken under. */
  at: number;
  params: Record<string, string>;
}

export type FromWorker =
  | { type: "channels"; channels: ChannelInfo[]; instruments: InstrumentInfo[] }
  | { type: "acq"; acq: Acquisition }
  | { type: "data"; series: Record<string, { t: Float64Array; v: Float64Array }>; latest: Record<string, Latest> }
  | { type: "status"; status: Status }
  // The last panel.json the stream carried, and its sha256: the rack's panel (§10.1).
  | { type: "panel"; text: string; sha: string }
  // A new connection: what came before belongs to a stream that has ended.
  | { type: "reset" };
