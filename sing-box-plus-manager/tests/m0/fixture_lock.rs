//! 输入摘要锁定（`docs/integration-contract.md` §2.2、§3）。
//!
//! 这一组用例是整个 M0 里唯一**面向上游**的门禁：它不验证主控做得对不对，
//! 只保证「主控当时按哪份节点侧输入验的」这件事被钉死。
//!
//! 摘要变化时它会红，修复方式是**核对变化并更新 `tests/node-fixtures.lock.toml`**，
//! 不是放宽断言。integration-contract §4 记的那次漂移（README 写 2、代码写 3）
//! 正是因为当时没有这样一道机械门禁，只靠人去读文档。

use std::collections::BTreeMap;
use std::process::Command;

use sha2::{Digest, Sha256};

use super::fixtures;

#[derive(Debug, serde::Deserialize)]
struct Lock {
    node: NodeLock,
    consumed: BTreeMap<String, String>,
    export: ExportLock,
}

#[derive(Debug, serde::Deserialize)]
struct NodeLock {
    repo: String,
    implementation_commit: String,
    schema_version: u32,
    snapshot_path: String,
    quota_path: String,
    #[allow(dead_code)]
    checked_at: String,
}

#[derive(Debug, serde::Deserialize)]
struct ExportLock {
    document_sha256: String,
}

fn lock() -> Lock {
    let path = fixtures::repo_root().join("tests").join("node-fixtures.lock.toml");
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("读取 {} 失败：{e}", path.display()));
    toml::from_str(&text).expect("锁文件应当是合法 TOML")
}

/// §2.2 三元组的第三项：对应链路的 fixture 摘要。
#[test]
fn 被消费的上游文件摘要与锁一致() {
    let lock = lock();
    let root = fixtures::sing_box_plus_dir();
    let mut drifted = Vec::new();
    for (relative, expected) in &lock.consumed {
        let path = root.join(relative);
        let bytes = std::fs::read(&path)
            .unwrap_or_else(|e| panic!("读取上游文件 {} 失败：{e}", path.display()));
        let actual = hex::encode(Sha256::digest(&bytes));
        if &actual != expected {
            drifted.push(format!("{relative}\n    锁定 {expected}\n    实际 {actual}"));
        }
    }
    assert!(
        drifted.is_empty(),
        "节点侧输入已变，主控这一侧尚未核对：\n{}\n\n\
         按 docs/integration-contract.md §2.2 核对变化后更新 tests/node-fixtures.lock.toml \
         的三元组（实现 commit + 协议版本 + 摘要）。\
         **不要**为了让用例变绿而放宽断言或在主控里加兼容分支。",
        drifted.join("\n")
    );

    // 导出器自己算的摘要也必须对得上。
    // 它读的目录由 SING_BOX_PLUS_DIR 决定，与本用例上面读的是同一个来源——
    // 一旦两边指到不同目录，这条会红，而逐文件摘要那一半仍可能全绿。
    assert_eq!(
        fixtures::fixtures().consumed,
        lock.consumed,
        "导出器与测试读到的上游文件不一致：检查 SING_BOX_PLUS_DIR"
    );
}

/// 导出文档整体的摘要。上游改了样例形状，这一条会先于逐文件摘要给出更直观的信号。
#[test]
fn 导出样例的摘要与锁一致() {
    let lock = lock();
    let actual = hex::encode(Sha256::digest(&fixtures::fixtures().raw));
    assert_eq!(
        actual, lock.export.document_sha256,
        "导出样例已变。核对后更新 tests/node-fixtures.lock.toml 的 export.document_sha256"
    );
}

/// §2.2 三元组的第一项：节点实现 commit。
///
/// 只记摘要不够——摘要能告诉你「变了」，但不能告诉你**变在哪一次提交**，
/// 而回报上游时需要的是后者。
#[test]
fn 锁里的节点实现commit与仓库实际一致() {
    let lock = lock();
    assert_eq!(lock.node.repo, "sing-box-plus");
    assert_eq!(
        lock.node.implementation_commit.len(),
        40,
        "implementation_commit 必须是完整的 40 位 SHA-1"
    );

    let mut command = Command::new("git");
    command.arg("-C").arg(fixtures::repo_root()).arg("log").arg("-1").arg("--format=%H").arg("--");
    // pathspec 相对 `-C` 的工作目录解析，所以这里用绝对路径，
    // 免得「跑在哪个目录下」变成一个会静默返回空结果的隐含前提。
    let node_dir = fixtures::sing_box_plus_dir();
    for relative in lock.consumed.keys() {
        command.arg(node_dir.join(relative));
    }
    let output = command.output().expect("运行 git log 失败：三元组的第一项无法核对");
    assert!(output.status.success(), "git log 失败：{}", String::from_utf8_lossy(&output.stderr));
    let actual = String::from_utf8_lossy(&output.stdout).trim().to_string();
    assert_eq!(
        actual, lock.node.implementation_commit,
        "节点实现 commit 已前进。按 §2.2 更新三元组：\
         同一个版本号下 fixture 可能已经变了，只更新摘要不够。"
    );
}

/// §2.2 三元组的第二项：协议版本。
///
/// 权威是节点侧 Go 常量，不是任何一份 README——
/// 导出器已经核过 Go 常量与 Python 模型一致，这里再把它钉进锁里。
#[test]
fn 协议版本三处一致且路径与之成对() {
    let lock = lock();
    let exported = fixtures::fixtures();
    assert_eq!(
        exported.schema_version, lock.node.schema_version,
        "导出的 schema_version 与锁不一致"
    );
    assert_eq!(
        proxy_manager::collect::SCHEMA_VERSION,
        lock.node.schema_version,
        "主控的 SCHEMA_VERSION 与节点侧 Go 常量不一致：\
         以代码为准并回报上游（docs/integration-contract.md §4）"
    );
    // 路径版本号与 schema_version 同步推进（C10）。
    assert_eq!(proxy_manager::collect::SNAPSHOT_PATH, lock.node.snapshot_path);
    assert_eq!(proxy_manager::quota::QUOTA_PATH, lock.node.quota_path);
    assert_eq!(lock.node.snapshot_path, format!("/v{}/snapshot", lock.node.schema_version));
    assert_eq!(lock.node.quota_path, format!("/v{}/quota", lock.node.schema_version));
}

/// health 闭集的四位必须与上游一致，且前三位/第四位的分组也一致。
///
/// 这一条是 C4 的**上游对齐**部分：主控写死了四个键名，
/// 一旦上游增删一位（那必然伴随 schema_version 推进），这里先红。
#[test]
fn health闭集与上游一致() {
    let exported = fixtures::fixtures();
    let mut expected = proxy_manager::collect::snapshot::HEALTH_KEYS.to_vec();
    expected.sort_unstable();
    assert_eq!(exported.health_keys, expected, "health 闭集与上游不一致");
    assert_eq!(exported.health_keys.len(), 4, "health 是恰好四位的闭集（C4）");

    let mut billing = proxy_manager::collect::snapshot::BILLING_HEALTH_KEYS.to_vec();
    billing.sort_unstable();
    assert_eq!(
        exported.billing_health_keys, billing,
        "不可入账的三位与上游不一致：audit_dropped 不在其中，它告警但照常入账"
    );
    assert!(!exported.billing_health_keys.contains(&"audit_dropped".to_string()));
}

/// 四向计数字段与上游一致。
#[test]
fn 四向计数字段与上游一致() {
    let exported = fixtures::fixtures();
    assert_eq!(
        exported.counter_fields,
        vec![
            "tcp_uplink_bytes".to_string(),
            "tcp_downlink_bytes".to_string(),
            "udp_uplink_bytes".to_string(),
            "udp_downlink_bytes".to_string(),
        ]
    );
}
