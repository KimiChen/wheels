//! 上游样例的装载入口。
//!
//! 样例**不 checked-in**：由 `scripts/export-node-fixtures.py` 在测试启动时从
//! `sing-box-plus` 的锁定入口导出（`docs/integration-contract.md` §2.3）。
//! 复制粘贴出来的第二份样例不会因为上游改形状而变红，那正是这条纪律要防的事。
//!
//! **缺 python3 或缺兄弟仓库时判失败，不跳过**：README §8 的措辞纪律写明
//! 「skip 不等于 pass」，把「环境不支持」的跳过当成通过，等于把一整类覆盖悄悄记成绿的。

// 两个测试二进制共用这个文件：m0 用全部字段，m1 只用其中几个。
#![allow(dead_code)]

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::process::Command;
use std::sync::OnceLock;

use serde::Deserialize;

#[derive(Debug, Deserialize)]
pub struct NodeFixtures {
    /// 以节点侧 Go 常量 `internal/userstats/snapshot.go` 的 `SchemaVersion` 为准。
    pub schema_version: u32,
    pub health_keys: Vec<String>,
    pub billing_health_keys: Vec<String>,
    pub counter_fields: Vec<String>,
    pub base_snapshot: serde_json::Value,
    /// 被消费的上游文件 → SHA-256。
    pub consumed: BTreeMap<String, String>,
    /// 导出文档的原始字节，用于摘要锁定。
    #[serde(skip)]
    pub raw: Vec<u8>,
}

pub fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

pub fn sing_box_plus_dir() -> PathBuf {
    match std::env::var_os("SING_BOX_PLUS_DIR") {
        Some(value) if !value.is_empty() => PathBuf::from(value),
        _ => repo_root().parent().expect("sing-box-plus-manager 应有父目录").join("sing-box-plus"),
    }
}

fn python() -> String {
    std::env::var("PYTHON").ok().filter(|v| !v.is_empty()).unwrap_or_else(|| "python3".into())
}

/// 整个测试二进制只导出一次。
pub fn fixtures() -> &'static NodeFixtures {
    static CELL: OnceLock<NodeFixtures> = OnceLock::new();
    CELL.get_or_init(|| {
        let script = repo_root().join("scripts").join("export-node-fixtures.py");
        let node_dir = sing_box_plus_dir();
        let output = Command::new(python())
            .arg(&script)
            .arg("--sing-box-plus")
            .arg(&node_dir)
            .output()
            .unwrap_or_else(|error| {
                panic!(
                    "无法运行样例导出器（{}）：{error}。\n\
                     夹具必须消费 sing-box-plus 的样例入口，不能退化成跳过——skip 不等于 pass。",
                    script.display()
                )
            });
        assert!(
            output.status.success(),
            "样例导出失败（退出码 {:?}）：{}\n节点侧目录：{}",
            output.status.code(),
            String::from_utf8_lossy(&output.stderr),
            node_dir.display()
        );
        let mut parsed: NodeFixtures =
            serde_json::from_slice(&output.stdout).expect("样例导出器的输出应当是合法 JSON");
        parsed.raw = output.stdout;
        parsed
    })
}

/// 取一份全新的基线快照副本。
pub fn base_snapshot() -> serde_json::Value {
    fixtures().base_snapshot.clone()
}
