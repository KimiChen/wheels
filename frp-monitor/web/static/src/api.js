// SPDX-License-Identifier: Apache-2.0
// 后端 API 封装。接口契约见 web/README.md；401 载荷 {"error":"unauthorized"}，
// 404 载荷 {"error":"not_found"}。

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message ?? code ?? `HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

async function request(path, { method = "GET", body } = {}) {
  let res;
  try {
    res = await fetch(path, {
      method,
      credentials: "same-origin",
      headers:
        body !== undefined ? { "Content-Type": "application/json" } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new ApiError(0, "network", "网络请求失败，请检查连接");
  }
  let data = null;
  const text = await res.text();
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const code =
      data && typeof data.error === "string" ? data.error : `http_${res.status}`;
    throw new ApiError(res.status, code);
  }
  return data;
}

export const publicApi = {
  overview: () => request("/api/public/v1/overview"),
  nodes: () => request("/api/public/v1/nodes"),
  node: (id) => request(`/api/public/v1/nodes/${encodeURIComponent(id)}`),
  nodeMetrics: (id, range) =>
    request(
      `/api/public/v1/nodes/${encodeURIComponent(id)}/metrics?range=${encodeURIComponent(range)}`,
    ),
};

export const adminApi = {
  login: (password) =>
    request("/api/admin/v1/login", { method: "POST", body: { password } }),
  logout: () => request("/api/admin/v1/logout", { method: "POST" }),
  session: () => request("/api/admin/v1/session"),
  nodes: () => request("/api/admin/v1/nodes"),
  node: (id) => request(`/api/admin/v1/nodes/${encodeURIComponent(id)}`),
  trafficDaily: (id, days) =>
    request(
      `/api/admin/v1/nodes/${encodeURIComponent(id)}/traffic/daily?days=${encodeURIComponent(days)}`,
    ),
};
