import {cardHardwareText} from "./node-data.mjs";
import {planText, expiryText, dateTime} from "./node-settings.mjs";
import {expiryState} from "./admin-overview.mjs";

const address = field => field?.quality === "ok" && typeof field.value === "string" && field.value.trim() ? field.value.trim() : "—";

// One display model for directory rows; keep byte arithmetic in planText's BigInt path.
export function directoryRow(node, groups = [], now = Date.now()) {
  const summary = [`#${node.id}`, cardHardwareText({os: node.facts?.os})];
  if (node.settings?.is_public === false) summary.push("隐藏");
  const plan = planText(node.traffic_plan), unlimited = node.traffic_plan?.quota_bytes === null;
  const expires = node.settings?.expires_at_ms, expiry = expiryText(expires, now);
  return {
    summary: summary.join(" · "),
    groups: groups.filter(group => group.node_ids.includes(node.id)).map(group => group.name),
    session: ({online: "在线", offline: "离线", waiting: "等待"})[node.session] ?? "—",
    ipv4: address(node.facts?.ipv4), ipv6: address(node.facts?.ipv6),
    plan: {
      text: unlimited ? "∞" : plan.percent,
      meter: unlimited ? null : plan.meter,
      title: node.traffic_plan ? `已用 ${plan.used} / ${plan.quota} · ${plan.note}` : "套餐数据暂不可用"
    },
    expires: {text: expiry ? expiry.replace(/后到期$/, "后") : expires == null ? "未设置" : "—", title: expiry ? dateTime(expires) : "未设置有效到期时间", state: expiryState(node, now)}
  };
}
