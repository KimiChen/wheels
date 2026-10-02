const write = (element, value) => { if (element.textContent !== value) element.textContent = value; };

// Keep brief loads quiet without hiding a connection failure during retries.
export function createConnectionStatus({notice}) {
  return {
    update(state, message = "") {
      if (state === "loading") return;
      if (state === "live") notice.hidden = true;
      else { write(notice, message); notice.hidden = false; }
    },
  };
}
