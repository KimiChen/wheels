//! `audit_dropped` 的旁路记录。
//!
//! 这一位是快照 health 的第四位，**粘滞到 plus 重启**，而 `snapshot_batches`
//! 根本不存它——settle 里只打了一条 `tracing::warn!`，日志一转就没了。
//!
//! 它与 `ev=gap` 不是一回事，这个区别决定了它为什么更该被看见：
//!
//! * `ev=gap` 是「丢了，而且写得下这句话」——队列满或总量封顶，可数、有边界，
//!   而且那句话就落在当事身份的文件里，查的时候自然看得到。
//! * `audit_dropped` 是「**连这句话都写不进去**」——打开失败、写失败、重开失败、
//!   轮转重命名失败、统计不了目录、总量触底无可删文件，以及 SIGHUP 重建 writer 时
//!   在途连接的投递。节点文档管那叫「一个没有任何痕迹的洞」。
//!
//! 因为它粘滞到重启，含义是「**从某一刻起到下次重启为止，这台节点的审计都不完整**」
//! ——范围是开口的。
//!
//! 每个 `(node, runtime_id)` 只记一次：粘滞到重启 ⟺ 换 runtime 才会有新的一段，
//! 所以一个 runtime 一条就够，去重键天然存在。**不改 schema**：这条记录属于审计链路，
//! 而审计链路与计费链路完全独立（C24）。

use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::error::{Error, Result};

pub const FILE: &str = "health.jsonl";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DroppedRecord {
    /// 主控**观察到**这一位的时刻，不是节点开始丢的时刻——后者没有任何东西知道。
    pub at: String,
    pub node_id: String,
    pub runtime_id: String,
    pub audit_dropped: bool,
}

/// 记一次 `audit_dropped`。已经记过这个 runtime 就什么都不做。
///
/// 返回 `true` 表示这一次真的写了，供调用方决定日志级别——
/// 每 60 秒一条同样的告警会把它自己淹掉。
pub fn note_dropped(dir: &Path, node_id: &str, runtime_id: &str, at: &str) -> Result<bool> {
    use std::io::Write;

    std::fs::create_dir_all(dir).map_err(|e| Error::Audit(format!("建审计目录：{e}")))?;
    let path = dir.join(FILE);
    for existing in read(dir)? {
        if existing.runtime_id == runtime_id {
            return Ok(false);
        }
    }
    let record = DroppedRecord {
        at: at.to_string(),
        node_id: node_id.to_string(),
        runtime_id: runtime_id.to_string(),
        audit_dropped: true,
    };
    let mut line = serde_json::to_vec(&record)
        .map_err(|e| Error::Audit(format!("序列化 health 记录：{e}")))?;
    line.push(b'\n');
    let mut file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(&path)
        .map_err(|e| Error::Audit(format!("打开 {}：{e}", path.display())))?;
    file.write_all(&line).map_err(|e| Error::Audit(format!("写 {}：{e}", path.display())))?;
    Ok(true)
}

/// 读出这台节点已记录的全部缺口。文件不存在时返回空——那是「没有缺口」。
///
/// 解析不了的行**跳过而不是整份失败**：这个文件的作用是让缺口可见，
/// 让一行坏数据把整份缺口记录变成不可读，正好是反的。
pub fn read(dir: &Path) -> Result<Vec<DroppedRecord>> {
    let path = dir.join(FILE);
    let bytes = match std::fs::read(&path) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(Error::Audit(format!("读 {}：{error}", path.display()))),
    };
    Ok(String::from_utf8_lossy(&bytes)
        .lines()
        .filter(|line| !line.trim().is_empty())
        .filter_map(|line| serde_json::from_str::<DroppedRecord>(line).ok())
        .collect())
}
