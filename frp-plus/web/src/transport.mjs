import {snapshot} from "./store.mjs";

// Public SSE cadence in monitor/service.go (separate from the agent report interval).
export const SNAPSHOT_REFRESH_SECONDS = 2;

// A reconnect always starts with an authoritative GET, then opens a new stream.
// Native EventSource retries are closed to prevent older snapshots racing a GET.
export function connect({onSnapshot, onState, fetcher = globalThis.fetch, Source = globalThis.EventSource, timer = globalThis.setTimeout, cancel = globalThis.clearTimeout}) {
  let generation = 0, stream = null, pending = null, watchdog = null, abort = null, stopped = false, attempts = 0;
  function clean() { stream?.close(); stream = null; cancel(pending); pending = null; cancel(watchdog); watchdog = null; abort?.abort(); abort = null; }
  function retry(state) {
    clean();
    onState(state);
    pending = timer(start, Math.min(30000, 1000 * 2 ** Math.min(attempts++, 5)));
  }
  async function start() {
    if (stopped) return;
    const current = ++generation;
    clean();
    onState("loading");
    const controller = new AbortController();
    abort = controller;
    const timeout = timer(() => controller.abort(), 10000);
    try {
      const response = await fetcher("/api/public/v1/nodes", {cache: "no-store", credentials: "same-origin", signal: abort.signal});
      if (!response.ok) throw new Error("snapshot unavailable");
      const data = snapshot(await response.json());
      if (stopped || current !== generation) return;
      onSnapshot(data);
      stream = new Source("/events/public");
      const own = stream;
      const armWatchdog = () => { cancel(watchdog); watchdog = timer(() => retry("disconnected"), 10000); };
      armWatchdog();
      stream.addEventListener("snapshot", event => {
        if (stopped || current !== generation || stream !== own) return;
        try { onSnapshot(snapshot(JSON.parse(event.data))); attempts = 0; armWatchdog(); onState("live"); }
        catch { retry("error"); }
      });
      stream.onerror = () => { if (current === generation && stream === own) retry("disconnected"); };
    } catch {
      if (!stopped && current === generation) retry("error");
    } finally { cancel(timeout); }
  }
  start();
  return {refresh: () => { attempts = 0; start(); }, stop: () => { stopped = true; generation++; clean(); }};
}
