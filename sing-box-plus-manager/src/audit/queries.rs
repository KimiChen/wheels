//! 「谁看过谁的明细」。
//!
//! §4.8 要求**拉取行为本身也落主控审计**。放文件不放库，与审计数据同一棵树、
//! 同一条纪律（C24）——也免得为一张小表去走 D32 的四步迁移。
//!
//! 记在**取数之前**：一次失败的查询同样是一次访问，而「查了但没查到」与「没查过」
//! 在这份日志里必须是两回事。写不下来就拒绝查询：这份日志是「谁看过谁的明细」的
//! 全部答案，悄悄放行等于让那个答案有一个没人知道的缺口。

use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};

pub const FILE: &str = "queries.jsonl";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct QueryRecord {
    pub at: String,
    /// 发起查询的人。
    pub actor: i64,
    /// 被查的人。自助查时两者相同。
    pub data_subject: i64,
    pub range: String,
}

pub fn record(root: &Path, entry: &QueryRecord) -> Result<()> {
    use std::io::Write;

    std::fs::create_dir_all(root).map_err(|e| Error::Audit(format!("建审计树根：{e}")))?;
    let path = root.join(FILE);
    let mut line =
        serde_json::to_vec(entry).map_err(|e| Error::Audit(format!("序列化查询记录：{e}")))?;
    line.push(b'\n');
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&path)
        .map_err(|e| Error::Audit(format!("打开 {}：{e}", path.display())))?;
    file.write_all(&line).map_err(|e| Error::Audit(format!("写 {}：{e}", path.display())))
}

/// 读回全部查询记录。坏行跳过而不是整份失败。
pub fn read(root: &Path) -> Result<Vec<QueryRecord>> {
    let path = root.join(FILE);
    let bytes = match std::fs::read(&path) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(Error::Audit(format!("读 {}：{error}", path.display()))),
    };
    Ok(String::from_utf8_lossy(&bytes)
        .lines()
        .filter(|line| !line.trim().is_empty())
        .filter_map(|line| serde_json::from_str::<QueryRecord>(line).ok())
        .collect())
}
