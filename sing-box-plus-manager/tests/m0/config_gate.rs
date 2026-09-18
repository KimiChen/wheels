//! 配置解析的门禁（C8、C12、C18、C3）。
//!
//! 这一组的失败模式都是**能跑起来的错**：配置被接受了，服务起来了，
//! 然后在结算或闸断链路上以别的形态爆出来。所以门禁要在启动路径上，
//! 而不是等到第一次采集或第一次下发。

use proxy_manager::config::{Config, FirstSnapshotPolicy};

const SERVER: &str = r#"
[listen]
api = "127.0.0.1:8173"
[storage]
path = "/var/lib/proxy-manager/proxy-manager.db"
[collect]
interval_secs = 60
min_interval_secs = 10
snapshot_max_age_secs = 120
[quota]
monthly_bytes = "322122547200"
display_timezone = "Asia/Shanghai"
"#;

fn nodes_with(extra: &str) -> String {
    format!(
        r#"
[[node]]
node_id = "node-example-01"
address = "node-01.example.com:8443"
snapshot_socket = "/run/sing-box-plus/user_stats.sock"
quota_socket = "/run/sing-box-plus/user_stats_quota.sock"
agent_spki_sha256 = "0000000000000000000000000000000000000000000000000000000000000000"
materials_dir = "/etc/proxy-manager/materials/node-example-01"
{extra}
"#
    )
}

// ---- 仓库里发出去的示例配置本身必须是合法的 ----

#[test]
fn 仓库里的示例配置能解析并通过全部校验() {
    let root = super::fixtures::repo_root();
    let config = Config::load_files(
        &root.join("config").join("server.example.toml"),
        &root.join("config").join("nodes.example.toml"),
    )
    .expect("示例配置必须能通过校验，否则它教给运维的就是错的");
    assert_eq!(config.nodes.len(), 1);
    assert_eq!(config.nodes[0].first_snapshot, FirstSnapshotPolicy::Baseline);
    assert!(config.nodes[0].quota_enabled());
    assert_eq!(config.server.quota.monthly_bytes().unwrap(), 322_122_547_200);
}

// ---- C8：首快照策略缺省即启动失败 ----

#[test]
fn 首快照策略缺省即失败() {
    let error = Config::from_str(SERVER, &nodes_with("")).unwrap_err();
    let message = error.to_string();
    assert!(
        message.contains("first_snapshot"),
        "错误必须指名缺的是 first_snapshot，实际：{message}"
    );
}

/// 反向证据：两种合法取值都必须被接受。
/// 否则上面那条会因为「任何 nodes.toml 都解析失败」而碰巧变绿。
#[test]
fn 首快照策略的两种取值都被接受() {
    for (literal, expected) in
        [("baseline", FirstSnapshotPolicy::Baseline), ("include", FirstSnapshotPolicy::Include)]
    {
        let config =
            Config::from_str(SERVER, &nodes_with(&format!(r#"first_snapshot = "{literal}""#)))
                .unwrap_or_else(|e| panic!("{literal} 应当被接受：{e}"));
        assert_eq!(config.nodes[0].first_snapshot, expected);
    }
    let error =
        Config::from_str(SERVER, &nodes_with(r#"first_snapshot = "whatever""#)).unwrap_err();
    assert!(error.to_string().contains("first_snapshot"));
}

// ---- C12：受限节点必须 deny ----

#[test]
fn 受限节点的startup_action必须是deny() {
    let allow = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "allow"
stale_after_secs = 600
stale_action = "deny""#,
    );
    let error = Config::from_str(SERVER, &allow).unwrap_err();
    let message = error.to_string();
    assert!(message.contains("C12"), "错误要带约束编号，实际：{message}");
    assert!(message.contains("startup_action"));
}

#[test]
fn 受限节点的过期策略必须显式且为deny() {
    for (extra, needle) in [
        (
            r#"enabled = true
startup_action = "deny"
stale_after_secs = 0
stale_action = "deny""#,
            "stale_after_secs",
        ),
        (
            r#"enabled = true
startup_action = "deny"
stale_after_secs = 600
stale_action = "allow""#,
            "stale_action",
        ),
    ] {
        let nodes =
            nodes_with(&format!("first_snapshot = \"baseline\"\n[node.quota_control]\n{extra}"));
        let error = Config::from_str(SERVER, &nodes).unwrap_err();
        assert!(error.to_string().contains(needle), "实际：{error}");
    }
}

/// 反向证据：未启用配额的节点不受这三条约束。
/// 否则「凡是有 quota_control 就报错」同样能让上面全绿。
#[test]
fn 未启用配额的节点不受deny约束() {
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = false
startup_action = "allow"
stale_after_secs = 0
stale_action = "allow""#,
    );
    let config = Config::from_str(SERVER, &nodes).expect("未启用配额时不该被拦");
    assert!(!config.nodes[0].quota_enabled());
}

// ---- C18：两个 socket 不许合并 ----

#[test]
fn 两个socket指向同一路径即失败() {
    let nodes = format!(
        r#"
[[node]]
node_id = "node-example-01"
address = "node-01.example.com:8443"
first_snapshot = "baseline"
snapshot_socket = "/run/sing-box-plus/user_stats.sock"
quota_socket = "/run/sing-box-plus/user_stats.sock"
agent_spki_sha256 = "{}"
materials_dir = "/etc/proxy-manager/materials/node-example-01"
"#,
        "0".repeat(64)
    );
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    let message = error.to_string();
    assert!(message.contains("C18"), "实际：{message}");
}

#[test]
fn socket必须是绝对路径() {
    let nodes = nodes_with(r#"first_snapshot = "baseline""#)
        .replace("/run/sing-box-plus/user_stats.sock", "run/user_stats.sock");
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    assert!(error.to_string().contains("绝对路径"), "实际：{error}");
}

// ---- 身份与解析纪律 ----

#[test]
fn node_id走节点侧同一条token规则() {
    for bad in ["", "有中文", "带 空格", &"x".repeat(129)] {
        let nodes = nodes_with(r#"first_snapshot = "baseline""#).replace("node-example-01", bad);
        assert!(
            Config::from_str(SERVER, &nodes).is_err(),
            "node_id {bad:?} 在节点侧会被拒，主控必须同样拒绝"
        );
    }
}

#[test]
fn node_id重复即失败() {
    let one = nodes_with(r#"first_snapshot = "baseline""#);
    let nodes = format!("{one}\n{one}");
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    assert!(error.to_string().contains("重复"), "实际：{error}");
}

/// 对未知字段失败关闭：宽容解析等于静默降级，
/// 一个拼错的键被忽略之后，运维看到的是「配置改了但没生效」。
#[test]
fn 未知字段失败关闭() {
    let nodes = nodes_with("first_snapshot = \"baseline\"\nunknown_key = 1");
    assert!(Config::from_str(SERVER, &nodes).is_err());
    let server = format!("{SERVER}\n[whatever]\nx = 1\n");
    assert!(Config::from_str(&server, &nodes_with(r#"first_snapshot = "baseline""#)).is_err());
}

/// C3：额度是 u64，TOML 整数是有符号 64 位，所以走十进制字符串。
#[test]
fn 额度走十进制字符串且覆盖u64上界() {
    let max = u64::MAX.to_string();
    let server = SERVER.replace("322122547200", &max);
    let config = Config::from_str(&server, &nodes_with(r#"first_snapshot = "baseline""#)).unwrap();
    assert_eq!(config.server.quota.monthly_bytes().unwrap(), u64::MAX);

    for bad in ["", "-1", "1.5", " 12", "12 ", "0x10", "18446744073709551616"] {
        let server = SERVER.replace("322122547200", bad);
        assert!(
            Config::from_str(&server, &nodes_with(r#"first_snapshot = "baseline""#)).is_err(),
            "monthly_bytes {bad:?} 必须被拒"
        );
    }
}

#[test]
fn 提频下限不得大于目标周期() {
    let server = SERVER.replace("min_interval_secs = 10", "min_interval_secs = 120");
    let error =
        Config::from_str(&server, &nodes_with(r#"first_snapshot = "baseline""#)).unwrap_err();
    assert!(error.to_string().contains("min_interval_secs"), "实际：{error}");
}
