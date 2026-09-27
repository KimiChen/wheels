import {history, windows} from "./history-data.mjs";

// One request per visible expanded node; superseded responses cannot change its graph.
export function connectHistory({nodeID, onData, onState, fetcher = globalThis.fetch, timer = globalThis.setTimeout, cancel = globalThis.clearTimeout}) {
  let selected = "1h", active = false, generation = 0, pending = null, timeout = null, controller = null;
  function clean() { cancel(pending); cancel(timeout); pending = timeout = null; controller?.abort(); controller = null; }
  async function refresh() {
    if (!active) return;
    clean();
    const current = ++generation, own = new AbortController(), window = selected;
    controller = own;
    onState("loading");
    timeout = timer(() => own.abort(), 10000);
    try {
      const response = await fetcher(`/api/public/v1/nodes/${encodeURIComponent(nodeID)}/history?window=${window}`, {cache: "no-store", credentials: "same-origin", signal: own.signal});
      if (!response.ok) throw new Error("history unavailable");
      const data = history(await response.json(), nodeID, window);
      if (!active || current !== generation) return;
      onData(data); onState("ready");
    } catch {
      if (active && current === generation) onState("error");
    } finally {
      if (active && current === generation) { cancel(timeout); timeout = null; controller = null; pending = timer(refresh, 30000); }
    }
  }
  return {
    activate(next) {
      if (next === active) return;
      active = next;
      if (active) refresh(); else { generation++; clean(); }
    },
    select(window) { if (!Object.hasOwn(windows, window) || selected === window) return; selected = window; refresh(); },
    refresh,
    stop() { active = false; generation++; clean(); },
  };
}
