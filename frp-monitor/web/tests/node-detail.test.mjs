import test from "node:test";
import assert from "node:assert/strict";
import {nodeID, nodeURL, hardwareValue, hardwareText, cpuCores, cpuLabel, cpuModel} from "../src/node-data.mjs";
import {diskPercent, resourceCharts, chart} from "../src/history-data.mjs";

const ok = value => ({quality: "ok", value});
test("node links and direct paths accept only positive SQLite integer identities", () => {
  assert.equal(nodeURL("1"), "/node/1");
  assert.equal(nodeURL("9223372036854775807"), "/node/9223372036854775807");
  assert.equal(nodeID("/node/123/"), "123");
  assert.equal(nodeID("/node/%31"), "1");
  for (const id of ["0", "01", "-1", "1.1", "9223372036854775808", "node_test-001", "../private", "test/node", "<script>", null, 1]) assert.equal(nodeURL(id), null);
  for (const path of ["/node/1/other", "/node/1%2f2", "/node/%E0%A4%A", "/admin/1", "/node/../1"]) assert.equal(nodeID(path), null);
});
test("hardware display projects only known fields and quality-ok values", () => {
  const hardware = {os: ok("Linux"), arch: ok("amd64"), virt: ok("kvm"), cpu_name: ok("Example CPU"), cpu_cores: ok(8), hostname: ok("private-host"), kernel: ok("private-kernel"), agent_version: ok("test")};
  assert.equal(hardwareText(hardware), "Linux · kvm · amd64");
  assert.equal(cpuLabel(hardware), "CPU 8 核");
  assert.equal(cpuModel(hardware), "Example CPU × 8");
  assert.equal(hardwareText(null), "系统信息待上报");
  assert.equal(hardwareValue({quality: "unavailable", value: "stale fact"}), "—");
  assert.equal(hardwareText({os: {quality: "unsupported", value: "Linux"}, arch: ok("arm64")}), "arm64");
  for (const value of [0, -1, 1.5, "8", Infinity, 4294967296]) assert.equal(cpuCores({cpu_cores: ok(value)}), null);
  assert.equal(cpuModel({cpu_name: ok(" "), cpu_cores: ok(2)}), "2 个逻辑核心");
  assert.equal(cpuLabel({}), "CPU");
});
test("disk history reads quality-counted fields with precise integer ratios", () => {
  const fields = (used, total, samples = 1) => ({disk_used: {value: used, samples}, disk_total: {value: total, samples}});
  assert.equal(diskPercent({fields: fields("9007199254740993", "18014398509481986")}), 50);
  assert.equal(diskPercent({fields: fields("0", "1")}), 0);
  for (const point of [{}, {fields: fields("1", "0")}, {fields: fields("2", "1")}, {fields: fields("1", "2", 0)}, {fields: fields("01", "2")}]) assert.equal(diskPercent(point), null);
  const spec = resourceCharts.find(item => item.key === "disk");
  const rows = [{at: "2026-09-27T11:58:00Z", samples: 1, fields: fields("0", "2")}, {at: "2026-09-27T11:59:00Z", samples: 1, fields: fields("1", "2", 0)}];
  const result = chart(rows, spec.series, {window: "1h", generatedAt: "2026-09-27T12:00:00Z", step: 60, ceiling: 100});
  assert.equal(result.paths[0].dots.length, 1);
  assert.equal(result.paths[0].dots[0].y, "110.00");
  assert.match(result.summaries[0], /0\.0%/);
});
