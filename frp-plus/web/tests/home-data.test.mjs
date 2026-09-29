import test from "node:test";
import assert from "node:assert/strict";
import {expiresSoon, normalizeHomeFilters, filterHomeNodes, sortHomeNodes, homeRowDetails} from "../src/home-data.mjs";
import {overview} from "../src/store.mjs";
import {dateTime} from "../src/node-settings.mjs";

const now = Date.UTC(2026, 8, 30, 12), day = 86400000;
const ok = value => ({quality: "ok", value});
const node = (id, session, expires, name = `Node ${id}`) => ({id, name, session,
  freshness: session === "online" ? "fresh" : "stale", groups: [{id: "2", name: "公开组"}],
  ...(expires === undefined ? {} : {billing: {expires_at_ms: expires}})});

test("seven-day expiry uses public integer timestamps and excludes already expired nodes", () => {
  for (const expires of [now + 1, now + day, now + 7 * day]) assert.equal(expiresSoon(node("1", "online", expires), now), true);
  for (const expires of [undefined, null, "1790769600000", NaN, Infinity, 8.64e15 + 1, now - 1, now, now + 7 * day + 1]) {
    assert.equal(expiresSoon(node("1", "online", expires), now), false);
  }
  const privateOnly = {...node("1", "online"), expires_at_ms: now + day, private_billing: {expires_at_ms: now + day}};
  assert.equal(expiresSoon(privateOnly, now), false);
});

test("state and expiry intersect group and name without disturbing ordering or global summaries", () => {
  const nodes = [node("1", "online", now + day, "East alpha"), node("2", "offline", now + 2 * day, "East beta"),
    node("3", "waiting", now + 3 * day, "East gamma"), node("4", "online", now + 20 * day, "East delta"),
    {...node("5", "online", now + day, "East epsilon"), groups: [{id: "3", name: "其他组"}]},
    node("6", "online", now + day, "West zeta")];
  assert.deepEqual(filterHomeNodes(nodes, {query: " east ", group: "group:2", status: "online", expiring: true}, now).map(n => n.id), ["1"]);
  assert.deepEqual(filterHomeNodes(nodes, {status: "offline"}, now).map(n => n.id), ["2"]);
  assert.deepEqual(filterHomeNodes(nodes, {status: "waiting"}, now).map(n => n.id), ["3"]);
  assert.deepEqual(filterHomeNodes(nodes, {group: "group:99", expiring: true}, now), []);
  assert.equal(overview(nodes).total, 6);
  assert.equal(nodes.filter(n => expiresSoon(n, now)).length, 5);
  assert.deepEqual(filterHomeNodes(nodes, {}, now), nodes);
});

test("stored filter values are limited to supported states and actual booleans", () => {
  for (const value of [null, undefined, "offline", {}, {status: "hidden", expiring: "true"}]) {
    assert.deepEqual(normalizeHomeFilters(value), {status: "all", expiring: false});
  }
  assert.deepEqual(normalizeHomeFilters({status: "waiting", expiring: true, group: "private"}), {status: "waiting", expiring: true});
});

test("expiry order is stable, includes expired nodes, and leaves unpublished dates last", () => {
  const nodes = [node("1", "online"), node("2", "online", now + day), node("3", "offline", now - day),
    node("4", "online", now + day), node("5", "online", "invalid")];
  assert.deepEqual(sortHomeNodes(nodes, "expiry").map(n => n.id), ["3", "2", "4", "1", "5"]);
  assert.deepEqual(nodes.map(n => n.id), ["1", "2", "3", "4", "5"]);
});

test("existing metric ordering keeps BigInt precision and missing samples last", () => {
  const nodes = [node("1", "online"), {...node("2", "online"), metrics: {net_rx_total: ok("9007199254740992")}},
    {...node("3", "online"), metrics: {net_rx_total: ok("9007199254740993")}}];
  assert.deepEqual(sortHomeNodes(nodes, "download-total").map(n => n.id), ["3", "2", "1"]);
  assert.deepEqual(sortHomeNodes(nodes, "default"), nodes);
});

test("row rates convert bytes per second to decimal Mbps with exact two-place rounding", () => {
  const rate = raw => homeRowDetails({metrics: {net_rx: ok(raw)}}).values;
  for (const [raw, number] of [["0", "0.00"], ["624", "0.00"], ["625", "0.01"], ["123456", "0.99"], ["125000", "1.00"], ["10000000", "80.00"]]) {
    assert.equal(rate(raw)["row-rx-rate"], number);
    assert.equal(rate(raw)["row-rx-rate-unit"], "Mbps");
  }
  const directions = homeRowDetails({metrics: {net_rx: ok("125000"), net_tx: ok("250000")}});
  assert.equal(directions.values["row-rx-rate"], "1.00");
  assert.equal(directions.values["row-tx-rate"], "2.00");
  assert.equal(directions.values["row-tx-rate-unit"], "Mbps");
});

test("row network conversion retains uint64 precision without floating point intermediates", () => {
  const details = homeRowDetails({metrics: {
    net_rx: ok("18446744073709551615"), net_tx: ok("9007199254740993"),
    net_rx_total: ok("18446744073709551615"), net_tx_total: ok("9007199254740993"),
  }});
  assert.equal(details.values["row-rx-rate"], "147573952589676.41");
  assert.equal(details.values["row-tx-rate"], "72057594037.93");
  assert.equal(details.values["row-rx-total"], "18.4");
  assert.equal(details.values["row-rx-total-unit"], "EB");
  assert.equal(details.values["row-tx-total"], "9.0");
  assert.equal(details.values["row-tx-total-unit"], "PB");
});

test("row transfer totals use decimal units and carry rounded boundaries without confusing GB with GiB", () => {
  for (const [raw, number, unit] of [["0", "0", "B"], ["999", "999", "B"], ["1000", "1.0", "KB"],
    ["1234", "1.2", "KB"], ["999950", "1.0", "MB"], ["1000000000", "1.0", "GB"],
    ["1073741824", "1.1", "GB"], ["1000000000000", "1.0", "TB"]]) {
    const details = homeRowDetails({metrics: {net_rx_total: ok(raw)}});
    assert.equal(details.values["row-rx-total"], number);
    assert.equal(details.values["row-rx-total-unit"], unit);
  }
});

test("row network values preserve unknown samples and genuine zero without using unrelated counters", () => {
  const fields = ["net_rx", "net_tx", "net_rx_total", "net_tx_total"];
  const keys = ["row-rx-rate", "row-tx-rate", "row-rx-total", "row-tx-total"];
  for (const field of [undefined, null, ok(null), ok(0), ok("-1"), ok("18446744073709551616"), {quality: "unavailable", value: "0"}]) {
    const details = homeRowDetails({metrics: Object.fromEntries(fields.map(key => [key, field]))});
    for (const key of keys) {
      assert.equal(details.values[key], "—");
      assert.equal(details.values[`${key}-unit`], "");
    }
  }
  for (const value of [undefined, null, {}, {metrics: {net_in_transfer: ok("1000"), net_out_transfer: ok("2000")}, traffic_today: {rx_bytes: "3000", tx_bytes: "4000"}, traffic_plan: {used_bytes: "5000"}}]) {
    const details = homeRowDetails(value);
    for (const key of keys) assert.equal(details.values[key], "—");
    assert.equal(details.values["row-uptime"], "—");
    assert.equal(details.values["row-expiry"], "—");
  }
  const zeros = homeRowDetails({metrics: Object.fromEntries([...fields, "uptime"].map(key => [key, ok("0")]))});
  assert.equal(zeros.values["row-rx-rate"], "0.00");
  assert.equal(zeros.values["row-tx-total"], "0");
  assert.equal(zeros.values["row-uptime"], "0 分钟");
});

test("row totals and uptime retain last valid offline samples with accurate descriptions", () => {
  const details = homeRowDetails({session: "offline", freshness: "stale", metrics: {
    net_rx: ok("125000"), net_rx_total: ok("1000"), net_tx_total: ok("2000"), uptime: ok("90061"),
  }});
  assert.equal(details.values["row-rx-rate"], "1.00");
  assert.equal(details.values["row-rx-total"], "1.0");
  assert.equal(details.values["row-tx-total"], "2.0");
  assert.equal(details.values["row-uptime"], "1 天 1 小时");
  assert.match(details.titles["row-rx-rate"], /最近有效采样/);
  assert.match(details.titles["row-rx-total"], /系统累计接收.*重启可能归零/);
  assert.match(details.titles["row-tx-total"], /系统累计发送.*重启可能归零/);
  assert.match(details.titles["row-uptime"], /最近有效采样.*不代表监控连续在线/);
});

test("row expiry uses only public billing, local dates and exact expired or seven-day urgency boundaries", () => {
  const localBoundary = new Date(2026, 0, 2, 0, 30, 15).getTime();
  const local = homeRowDetails({billing: {expires_at_ms: localBoundary}}, localBoundary - 1);
  assert.equal(local.values["row-expiry"], "2026-01-02");
  assert.equal(local.titles["row-expiry"], `到期时间：${dateTime(localBoundary)}（浏览器本地时区） · 1 天后到期`);
  for (const expires of [now - day, now, now + 1, now + 7 * day]) {
    const details = homeRowDetails({billing: {expires_at_ms: expires}}, now);
    assert.equal(details.expiryUrgent, true);
    assert.match(details.titles["row-expiry"], expires <= now ? /已到期/ : /天后到期/);
  }
  assert.equal(homeRowDetails({billing: {expires_at_ms: now + 7 * day + 1}}, now).expiryUrgent, false);
  for (const expires of [undefined, null, "0", NaN, Infinity, Number.MAX_SAFE_INTEGER]) {
    const details = homeRowDetails({billing: {expires_at_ms: expires}, expires_at_ms: now, private_billing: {expires_at_ms: now}}, now);
    assert.equal(details.values["row-expiry"], "—");
    assert.equal(details.expiryUrgent, false);
  }
});
