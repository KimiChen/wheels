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

// ---- C12：受限节点可以失败开放，但必须签字 ----
//
// 这一组原本钉的是「两个动作都必须是 deny」。放宽是一次有意的产品决定
// （额度用于观察用量而非强制封顶），所以这些用例改写而不是删除：
// 要证明 allow 现在能启动，**并且**证明它没有被悄悄放进去。

#[test]
fn 失败开放没有签字要被拒() {
    for action in ["startup_action", "stale_action"] {
        let mut block = String::from(
            "first_snapshot = \"baseline\"\n[node.quota_control]\nenabled = true\nstale_after_secs = 600\n",
        );
        block.push_str(&format!("{action} = \"allow\"\n"));
        block.push_str(if action == "startup_action" {
            "stale_action = \"deny\"\n"
        } else {
            "startup_action = \"deny\"\n"
        });

        let error = Config::from_str(SERVER, &nodes_with(&block)).unwrap_err();
        let message = error.to_string();
        assert!(message.contains("C12"), "错误要带约束编号，实际：{message}");
        assert!(
            message.contains("fail_open_acknowledged"),
            "错误要指出缺的是签字，实际：{message}"
        );
    }
}

#[test]
fn 签了字的失败开放可以启动并且状态可见() {
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "allow"
stale_after_secs = 600
stale_action = "allow"
fail_open_acknowledged = "额度用于观察用量，不接受控制面成为转发单点""#,
    );
    let config = Config::from_str(SERVER, &nodes).expect("签过字的失败开放应当能启动");
    // 只能启动还不够：这个状态必须能被读出来交给 API，否则它只存在于 TOML 里。
    assert_eq!(
        config.nodes[0].fail_open_reason(),
        Some("额度用于观察用量，不接受控制面成为转发单点")
    );
}

#[test]
fn 失败关闭时没有失败开放状态可报() {
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "deny"
stale_after_secs = 600
stale_action = "deny""#,
    );
    let config = Config::from_str(SERVER, &nodes).unwrap();
    assert_eq!(config.nodes[0].fail_open_reason(), None);
}

#[test]
fn 改回失败关闭却留着签字要被拒() {
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "deny"
stale_after_secs = 600
stale_action = "deny"
fail_open_acknowledged = "早就改回去了但忘了删""#,
    );
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    let message = error.to_string();
    assert!(message.contains("C12"), "实际：{message}");
    assert!(message.contains("fail_open_acknowledged"), "实际：{message}");
}

#[test]
fn 空白签字不算签字() {
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "allow"
stale_after_secs = 600
stale_action = "deny"
fail_open_acknowledged = "   ""#,
    );
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    assert!(error.to_string().contains("fail_open_acknowledged"), "实际：{error}");
}

#[test]
fn 未启用配额时不检查失败开放() {
    // 只采集不下发的节点（切换阶段 P3 的形态）本来就不谈额度，
    // 不该因为 allow 而拒绝启动。
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = false
startup_action = "allow"
stale_after_secs = 600
stale_action = "allow""#,
    );
    let config = Config::from_str(SERVER, &nodes).expect("未启用配额的节点不该被 C12 拦下");
    assert_eq!(config.nodes[0].fail_open_reason(), None);
}

#[test]
fn 受限节点的过期期限必须显式() {
    // 这一条与失败开放无关：没有期限就不存在「过期」这个事件。
    let nodes = nodes_with(
        r#"first_snapshot = "baseline"
[node.quota_control]
enabled = true
startup_action = "deny"
stale_after_secs = 0
stale_action = "deny""#,
    );
    let error = Config::from_str(SERVER, &nodes).unwrap_err();
    assert!(error.to_string().contains("stale_after_secs"), "实际：{error}");
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

// C3（额度走十进制字符串、覆盖 u64 上界）原本在这里，钉的是配置里的
// quota.monthly_bytes。分档之后那个字段已删除，这条纪律的载体变成
// quota_groups 的定宽文本列与它的 i64 上界 CHECK，用例相应搬到
// tests/m2/settings.rs——那里能真的写一次库，证明超界值写不进去。

#[test]
fn 提频下限不得大于目标周期() {
    let server = SERVER.replace("min_interval_secs = 10", "min_interval_secs = 120");
    let error =
        Config::from_str(&server, &nodes_with(r#"first_snapshot = "baseline""#)).unwrap_err();
    assert!(error.to_string().contains("min_interval_secs"), "实际：{error}");
}
