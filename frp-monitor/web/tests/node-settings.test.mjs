import test from "node:test";
import assert from "node:assert/strict";
import {billingText, todayText, planText, gibBytes, gibInput, priceMinor, priceInput, settingsRequest, localDateInput} from "../src/node-settings.mjs";

const form = {name: "测试节点", public_note: "公开", private_note: "私有", is_public: true, publish_billing: false, publish_traffic_plan: true,
  price: "12.34", currency: "CNY", billing_cycle: "月付", expires_at_ms: "", renewal_note: "人工续费",
  traffic_quota: "1024", traffic_mode: "max", traffic_reset_mode: "monthly", traffic_reset_day: "31", traffic_reset_timezone: "Asia/Shanghai", traffic_used: ""};

test("current traffic uses the snapshot independently of optional history", () => {
  const today = todayText({day: "2026-09-28", timezone: "Asia/Shanghai", rx_bytes: "0", tx_bytes: "1073741824", partial: true});
  assert.equal(today.rx, "0 B"); assert.equal(today.tx, "1.0 GiB");
  assert.match(today.note, /主控时区 Asia\/Shanghai/); assert.match(today.note, /不完整/);
  assert.equal(todayText(null).rx, "—");
  assert.equal(todayText({rx_bytes: 1}).rx, "—");
});
test("quota keeps zero, missing limits and overage distinct with exact integers", () => {
  assert.equal(planText({used_bytes: "0", quota_bytes: null}).quota, "未设上限");
  assert.equal(planText({used_bytes: "1", quota_bytes: "0"}).quota, "0 B");
  assert.equal(planText({used_bytes: "1", quota_bytes: "0"}).percent, "—");
  const plan = planText({used_bytes: "27021597764222979", quota_bytes: "18014398509481986", mode: "total", reset_mode: "manual", partial: false});
  assert.equal(plan.percent, "150.0%"); assert.match(plan.remaining, /^超出 /); assert.match(plan.note, /total · 手动重置/);
  assert.equal(planText({used_bytes: null, quota_bytes: "1"}).used, "—");
});
test("GiB conversion round-trips large exact byte totals without floating point", () => {
  for (const bytes of ["0", "1", "1073741824", "9007199254740993", "18446744073709551615"]) assert.equal(gibBytes(gibInput(bytes)), bytes);
  assert.equal(gibBytes("1.5"), "1610612736");
  for (const input of ["-1", "1e9", "Infinity", "1,000", "", "00", "0.1<script>"]) assert.throws(() => gibBytes(input));
});
test("money inputs respect currency minor units and preserve large integers", () => {
  assert.equal(priceMinor("12.34", "CNY"), "1234"); assert.equal(priceInput("1234", "CNY"), "12.34");
  assert.equal(priceMinor("123", "JPY"), "123"); assert.equal(priceMinor("1.234", "KWD"), "1234");
  assert.equal(priceMinor(priceInput("9007199254740993", "CNY"), "CNY"), "9007199254740993");
  assert.equal(priceMinor("", "CNY"), null); assert.equal(priceMinor("0", "CNY"), "0");
  for (const [value, currency] of [["1.001", "CNY"], ["1.1", "JPY"], ["-1", "USD"], ["1", "not-a-currency"], ["9223372036854775808", "JPY"]]) assert.throws(() => priceMinor(value, currency));
  assert.match(billingText({price_minor: "0", currency: "CNY", billing_cycle: "月付", expires_at_ms: null}), /0 CNY \/ 月付/);
  assert.equal(billingText(null), "");
});
test("saving settings leaves current usage untouched unless the administrator enters calibration", () => {
  const request = settingsRequest(form, 3);
  assert.equal(request.config_revision, 3); assert.equal(request.traffic_quota_bytes, "1099511627776");
  assert.equal(request.price_minor, "1234"); assert.equal(request.publish_billing, false); assert.equal(request.private_note, "私有");
  assert.equal(Object.hasOwn(request, "traffic_used_bytes"), false);
  const changedMode = settingsRequest({...form, traffic_mode: "total"}, 3);
  assert.equal(Object.hasOwn(changedMode, "traffic_used_bytes"), false);
  const calibrated = settingsRequest({...form, traffic_used: "150"}, 3);
  assert.equal(calibrated.traffic_used_bytes, "161061273600");
  assert.equal(settingsRequest({...form, traffic_used: "0"}, 3).traffic_used_bytes, "0");
  const cleared = settingsRequest({...form, price: "", traffic_quota: ""}, 3);
  assert.equal(cleared.price_minor, null); assert.equal(cleared.currency, null); assert.equal(cleared.traffic_quota_bytes, null);
});
test("settings reject invalid timezone, date, quota and missing revision", () => {
  for (const change of [{traffic_reset_day: "0"}, {traffic_reset_day: "32"}, {traffic_reset_timezone: "invalid/timezone"}, {traffic_mode: "sum"}, {traffic_reset_mode: "yearly"}, {expires_at_ms: "bad date"}, {name: ""}, {traffic_used: "-1"}]) assert.throws(() => settingsRequest({...form, ...change}, 3));
  assert.throws(() => settingsRequest(form, null));
  assert.equal(localDateInput(null), "");
  const date = new Date("2026-09-28T00:00:00Z");
  assert.equal(new Date(localDateInput(date.getTime())).getTime(), date.getTime());
});
