const labels = {loading: "正在同步", live: "已连接", disconnected: "实时连接中断", error: "暂时无法同步"};
const write = (element, value) => { if (element.textContent !== value) element.textContent = value; };

// Keep brief loads quiet without hiding a connection failure during retries.
export function createConnectionStatus({status, label, notice, timer = globalThis.setTimeout, cancel = globalThis.clearTimeout}) {
  let pending = null;
  function clear() { cancel(pending); pending = null; }
  function show(state) { status.dataset.state = state; write(label, labels[state]); }
  return {
    update(state, message = "") {
      clear();
      if (state === "loading") {
        if (notice.hidden) pending = timer(() => { pending = null; show("loading"); }, 300);
        return;
      }
      show(state);
      if (state === "live") notice.hidden = true;
      else { write(notice, message); notice.hidden = false; }
    },
    stop: clear,
  };
}
