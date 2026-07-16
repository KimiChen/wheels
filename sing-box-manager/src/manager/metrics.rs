//! 系统指标：一次 [`store::observability::metrics_snapshot`] 计算，两种渲染（Prometheus 文本 / JSON）。
//! 不引第三方 exporter——Prometheus 文本就是拼 `# HELP/# TYPE/name value`。标签仅状态枚举（基数有界）。

use std::fmt::Write;

use crate::store::observability::MetricsSnapshot;

fn line(out: &mut String, name: &str, help: &str, ty: &str, value: i64) {
    let _ = writeln!(out, "# HELP {name} {help}");
    let _ = writeln!(out, "# TYPE {name} {ty}");
    let _ = writeln!(out, "{name} {value}");
}

fn labeled(
    out: &mut String,
    name: &str,
    help: &str,
    ty: &str,
    pairs: &[(String, i64)],
    label: &str,
) {
    let _ = writeln!(out, "# HELP {name} {help}");
    let _ = writeln!(out, "# TYPE {name} {ty}");
    for (k, v) in pairs {
        let _ = writeln!(out, "{name}{{{label}=\"{}\"}} {v}", escape(k));
    }
}

fn escape(s: &str) -> String {
    s.replace('\\', "\\\\").replace('"', "\\\"")
}

/// 渲染 Prometheus 文本。`version`=构建版本；`uptime`=进程运行秒；`now`=当前 unix。
pub fn render_prometheus(m: &MetricsSnapshot, version: &str, uptime: i64, now: i64) -> String {
    let mut o = String::with_capacity(2048);
    let _ = writeln!(o, "# HELP sbm_build_info Build info.");
    let _ = writeln!(o, "# TYPE sbm_build_info gauge");
    let _ = writeln!(o, "sbm_build_info{{version=\"{}\"}} 1", escape(version));
    line(
        &mut o,
        "sbm_schema_version",
        "Applied schema migration version.",
        "gauge",
        m.schema_version,
    );
    line(
        &mut o,
        "sbm_uptime_seconds",
        "Process uptime seconds.",
        "gauge",
        uptime,
    );
    line(
        &mut o,
        "sbm_time_unixtime",
        "Server unix time.",
        "gauge",
        now,
    );
    line(
        &mut o,
        "sbm_hosts_total",
        "Total hosts.",
        "gauge",
        m.hosts_total,
    );
    line(
        &mut o,
        "sbm_entries_total",
        "Total entries.",
        "gauge",
        m.entries_total,
    );
    line(
        &mut o,
        "sbm_nodes_total",
        "Total nodes.",
        "gauge",
        m.nodes_total,
    );
    line(
        &mut o,
        "sbm_landings_total",
        "Total landings.",
        "gauge",
        m.landings_total,
    );
    labeled(
        &mut o,
        "sbm_routes",
        "Routes by status.",
        "gauge",
        &m.routes_by_status,
        "status",
    );
    line(
        &mut o,
        "sbm_agents_total",
        "Total agents.",
        "gauge",
        m.agents_total,
    );
    line(
        &mut o,
        "sbm_agents_online",
        "Agents polled within freshness window.",
        "gauge",
        m.agents_online,
    );
    line(
        &mut o,
        "sbm_agents_trusted",
        "Agents with trusted certificate.",
        "gauge",
        m.agents_trusted,
    );
    line(
        &mut o,
        "sbm_users_total",
        "Total users.",
        "gauge",
        m.users_total,
    );
    line(
        &mut o,
        "sbm_users_effective_disabled",
        "Users disabled by quota/expiry.",
        "gauge",
        m.users_effective_disabled,
    );
    line(
        &mut o,
        "sbm_users_over_quota",
        "Users over quota.",
        "gauge",
        m.users_over_quota,
    );
    labeled(
        &mut o,
        "sbm_deployments",
        "Deployments by status.",
        "gauge",
        &m.deployments_by_status,
        "status",
    );
    line(
        &mut o,
        "sbm_entries_stale",
        "Entries with stale metering.",
        "gauge",
        m.entries_stale,
    );
    line(
        &mut o,
        "sbm_usage_uplink_bytes",
        "Sum of accounted uplink bytes.",
        "counter",
        m.usage_uplink_bytes,
    );
    line(
        &mut o,
        "sbm_usage_downlink_bytes",
        "Sum of accounted downlink bytes.",
        "counter",
        m.usage_downlink_bytes,
    );
    line(
        &mut o,
        "sbm_alerts_firing",
        "Active firing alerts.",
        "gauge",
        m.alerts_firing,
    );
    o
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prometheus_text_has_type_and_bounded_labels() {
        let m = MetricsSnapshot {
            schema_version: 8,
            hosts_total: 3,
            routes_by_status: vec![("active".into(), 2), ("draft".into(), 1)],
            deployments_by_status: vec![("succeeded".into(), 5)],
            ..Default::default()
        };
        let txt = render_prometheus(&m, "0.1.0", 42, 1_700_000_000);
        assert!(txt.contains("# TYPE sbm_hosts_total gauge"));
        assert!(txt.contains("sbm_hosts_total 3"));
        assert!(txt.contains("sbm_build_info{version=\"0.1.0\"} 1"));
        assert!(txt.contains("sbm_routes{status=\"active\"} 2"));
        assert!(txt.contains("sbm_schema_version 8"));
        // 无 per-user / 无无界标签。
        assert!(!txt.contains("user=\""));
    }
}
