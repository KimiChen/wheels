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

test("row runtime uses system uptime and last valid counters without rounding or relabeling them as active connections", () => {
  const details = homeRowDetails({session: "offline", freshness: "stale", metrics: {
    uptime: ok("90061"), load: ok([0, 1.2, 2.34]), tcp: ok("9007199254740993"), udp: ok("0"), procs: ok("0"),
  }});
  assert.equal(details.values["runtime-uptime"], "1 天 1 小时");
  assert.equal(details.values["runtime-load"], "0.00 / 1.20 / 2.34");
  assert.equal(details.values["runtime-connections"], "9007199254740993 / 0");
  assert.equal(details.values["runtime-processes"], "0");
  assert.match(details.titles["runtime-connections"], /最近有效采样/);
  assert.doesNotMatch(details.titles["runtime-connections"], /active|活跃/);
});

test("row detail keeps missing or invalid samples unknown while displaying genuine zero and partial daily totals", () => {
  for (const value of [undefined, null, {}, {metrics: {uptime: ok(0), load: ok([0, 1]), tcp: ok("-1"), udp: {quality: "unavailable", value: "0"}, procs: ok("18446744073709551616")}}]) {
    const details = homeRowDetails(value);
    for (const key of ["runtime-uptime", "runtime-load", "runtime-connections", "runtime-processes", "row-today"]) assert.equal(details.values[key], "—");
  }
  const partial = homeRowDetails({metrics: {tcp: ok("0")}, traffic_today: {day: "2026-09-30", timezone: "Asia/Shanghai", rx_bytes: "0", tx_bytes: "1073741824", partial: true}});
  assert.equal(partial.values["runtime-connections"], "0 / —");
  assert.equal(partial.values["row-today"], "0 B / 1.0 GiB");
  assert.equal(partial.titles["row-today"], "2026-09-30 · 统计日时区：Asia/Shanghai");
  assert.equal(homeRowDetails({traffic_today: {rx_bytes: 0, tx_bytes: "0"}}).values["row-today"], "— / 0 B");
});

test("row quota balances preserve one-byte differences beyond Number precision and distinguish zero from unlimited", () => {
  const remaining = (used, quota) => homeRowDetails({traffic_plan: {used_bytes: used, quota_bytes: quota}}).values["row-plan-remaining"];
  assert.equal(remaining("9007199254740992", "9007199254740993"), "剩余 1 B");
  assert.equal(remaining("9007199254740993", "9007199254740992"), "超出 1 B");
  assert.equal(remaining("0", "0"), "剩余 0 B");
  assert.equal(remaining("1", "0"), "超出 1 B");
  assert.equal(remaining("0", null), "无限流量");
  assert.equal(remaining(undefined, null), "无限流量");
  for (const quota of [undefined, "bad", 0]) assert.equal(remaining("0", quota), "—");
  assert.equal(remaining(undefined, "1"), "—");
  assert.equal(homeRowDetails({traffic_plan: {used_bytes: "0", quota_bytes: null}}).values["row-plan-usage"], "0 B / ∞");
});

test("row plan uses known mode mappings and the server reset boundary, with manual reset taking precedence", () => {
  for (const [mode, label] of [["max", "收发取较大值"], ["total", "双向合计"], ["rx", "仅接收"], ["tx", "仅发送"], ["__proto__", "—"], [undefined, "—"]]) {
    assert.equal(homeRowDetails({traffic_plan: {mode}}).values["row-plan-mode"], label);
  }
  const details = homeRowDetails({traffic_plan: {reset_mode: "monthly", period_end_at_ms: now, reset_timezone: "America/New_York"}});
  assert.equal(details.values["row-plan-reset"], dateTime(now));
  assert.match(details.titles["row-plan-reset"], /浏览器本地时区/);
  assert.doesNotMatch(details.titles["row-plan-reset"], /America\/New_York/);
  const localBoundary = new Date(2026, 0, 2, 0, 30, 15).getTime();
  const compact = homeRowDetails({traffic_plan: {reset_mode: "monthly", period_end_at_ms: localBoundary}});
  assert.equal(compact.values["row-plan-reset-short"], "2026-01-02");
  assert.equal(compact.titles["row-plan-reset-short"], `下次重置：${dateTime(localBoundary)}（浏览器本地时区）`);
  assert.equal(compact.titles["row-plan-reset-short"], compact.titles["row-plan-reset"]);
  for (const value of [undefined, null, "0", NaN, Infinity, Number.MAX_SAFE_INTEGER]) {
    assert.equal(homeRowDetails({traffic_plan: {reset_mode: "monthly", period_end_at_ms: value}}).values["row-plan-reset"], "—");
    assert.equal(homeRowDetails({traffic_plan: {reset_mode: "monthly", period_end_at_ms: value}}).values["row-plan-reset-short"], "—");
    assert.equal(homeRowDetails({traffic_plan: {reset_mode: "manual", period_end_at_ms: value}}).values["row-plan-reset"], "手动重置");
    assert.equal(homeRowDetails({traffic_plan: {reset_mode: "manual", period_end_at_ms: value}}).values["row-plan-reset-short"], "手动重置");
  }
  assert.equal(homeRowDetails({traffic_plan: {reset_mode: "manual", period_end_at_ms: now}}).values["row-plan-reset"], "手动重置");
  assert.equal(homeRowDetails({traffic_plan: {reset_mode: "invalid", period_end_at_ms: now}}).values["row-plan-reset"], "—");
  assert.equal(homeRowDetails({traffic_plan: {reset_mode: "invalid", period_end_at_ms: now}}).values["row-plan-reset-short"], "—");
});

test("unpublished row plans do not infer public data from private fields or hide independent daily traffic", () => {
  for (const traffic_plan of [undefined, null, false, "hidden", []]) {
    const details = homeRowDetails({traffic_plan, traffic_quota_bytes: null, traffic_used_bytes: "0", private_traffic_plan: {used_bytes: "0", quota_bytes: null}, traffic_today: {rx_bytes: "0", tx_bytes: "0"}});
    assert.equal(details.planPublished, false);
    for (const key of ["row-plan-usage", "row-plan-remaining", "row-plan-mode", "row-plan-reset", "row-plan-reset-short"]) assert.equal(details.values[key], "—");
    assert.equal(details.titles["row-plan-usage"], "套餐未公开");
    assert.equal(details.values["row-today"], "0 B / 0 B");
  }
  assert.equal(homeRowDetails({traffic_plan: {}}).planPublished, true);
});
