import {select, sortNodes, sortOptions} from "./store.mjs";

export const homeSortOptions = [...sortOptions, ["expiry", "到期时间"]];
export const statusFilters = ["all", "online", "offline", "waiting"];
const week = 7 * 86400000;

function expiration(node) {
  const value = node.billing?.expires_at_ms;
  return Number.isSafeInteger(value) && !Number.isNaN(new Date(value).getTime()) ? value : null;
}

// Only the public billing projection may contribute to this count or filter.
// Expired nodes are not described as expiring within the next seven days.
export function expiresSoon(node, now = Date.now()) {
  const value = expiration(node);
  return value !== null && value > now && value <= now + week;
}

export function normalizeHomeFilters(value) {
  return {status: statusFilters.includes(value?.status) ? value.status : "all", expiring: value?.expiring === true};
}

export function filterHomeNodes(nodes, {query = "", group = "all", status = "all", expiring = false} = {}, now = Date.now()) {
  const filters = normalizeHomeFilters({status, expiring});
  return select(nodes, query, group).filter(node =>
    (filters.status === "all" || node.session === filters.status) && (!filters.expiring || expiresSoon(node, now)));
}

export function sortHomeNodes(nodes, order = "default") {
  if (order !== "expiry") return sortNodes(nodes, order);
  return [...nodes].sort((a, b) => {
    const left = expiration(a), right = expiration(b);
    if (left === null || right === null) return left === right ? 0 : left === null ? 1 : -1;
    return left < right ? -1 : left > right ? 1 : 0;
  });
}
