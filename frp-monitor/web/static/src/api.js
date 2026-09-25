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
  tunnels: () => request("/api/public/v1/tunnels"),
};

export const adminApi = {
  login: (password) =>
    request("/api/admin/v1/login", { method: "POST", body: { password } }),
  logout: () => request("/api/admin/v1/logout", { method: "POST" }),
  session: () => request("/api/admin/v1/session"),
  nodes: () => request("/api/admin/v1/nodes"),
  node: (id) => request(`/api/admin/v1/nodes/${encodeURIComponent(id)}`),
  nodeEvents: (id, limit) =>
    request(
      `/api/admin/v1/nodes/${encodeURIComponent(id)}/events?limit=${encodeURIComponent(limit)}`,
    ),
  trafficDaily: (id, days) =>
    request(
      `/api/admin/v1/nodes/${encodeURIComponent(id)}/traffic/daily?days=${encodeURIComponent(days)}`,
    ),
  credentials: () => request("/api/admin/v1/credentials"),
  createCredential: (id, comment) =>
    request("/api/admin/v1/credentials", {
      method: "POST",
      body: comment === "" ? { id } : { id, comment },
    }),
  deleteCredential: (id) =>
    request(`/api/admin/v1/credentials/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
};

/** 备份可用性探测：200 可下载；409 no_data_dir 表示服务端未开启持久化。 */
export async function probeBackup() {
  try {
    const res = await fetch("/api/admin/v1/backup", {
      credentials: "same-origin",
    });
    // 只需要状态码，立即取消避免真正下载文件体。
    res.body?.cancel().catch(() => {});
    if (res.status === 409) return "no_data_dir";
    if (res.status === 401) return "unauthorized";
    return res.ok ? "ok" : "unknown";
  } catch {
    return "unknown";
  }
}
