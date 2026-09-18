//! SQLite 无损数值编码（`docs/data-model.md` §3.1）。
//!
//! 起因是两条硬事实：SQLite 的 INTEGER 是**有符号** 64 位，装不下完整 u64；
//! TEXT 列一旦被 SQL 拿去做算术或 CAST，隐式数值转换会经 REAL 丢精度。
//! 于是存储契约固定为：**定宽左补零的十进制文本 + `COLLATE BINARY`**，
//! 所有算术在 Rust 里用 checked 运算完成。
//!
//! 定宽不是为了好看：它让同类型的二进制字典序等于整数序，
//! 排序、范围查询与索引因此不需要任何转换就保持整数语义。
//!
//! **禁止对这些列使用 SQL `SUM`、加减乘除或 CAST 到 INTEGER/REAL。**
//! 这条在迁移里由 CHECK 约束兜住一半（写坏的值进不去），另一半靠 code review。

use crate::error::{Error, Result};

/// u64 的十进制上界是 20 位（`18446744073709551615`）。
pub const U64_TEXT_WIDTH: usize = 20;
/// u128 的十进制上界是 39 位（`340282366920938463463374607431768211455`）。
pub const U128_TEXT_WIDTH: usize = 39;

/// 四向累计游标、sequence、generation、started_at_unix_ms、epoch、四向 delta。
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Default)]
pub struct U64Text(u64);

/// 多方向/多节点总和、周期累计、lifetime。
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Default)]
pub struct U128Text(u128);

impl U64Text {
    pub const MAX: U64Text = U64Text(u64::MAX);

    pub const fn new(value: u64) -> Self {
        U64Text(value)
    }

    pub const fn get(self) -> u64 {
        self.0
    }

    /// 绑定进 SQLite 用的定宽文本。
    pub fn encode(self) -> String {
        format!("{:0width$}", self.0, width = U64_TEXT_WIDTH)
    }

    /// 从库里读回。宽度不符一律拒绝——一个 19 位的值说明写入方绕过了这套编码，
    /// 而它在字典序里会排到所有 20 位值的前面，是个会静默出错的排序 bug。
    pub fn decode(text: &str) -> Result<Self> {
        decode_fixed_width(text, U64_TEXT_WIDTH)?
            .parse::<u64>()
            .map(U64Text)
            .map_err(|_| Error::Codec(format!("超出 u64 范围：{text}")))
    }

    /// 出到 HTTP API 与前端的形式：去掉补零的十进制字符串（README §6、C3）。
    pub fn to_api_string(self) -> String {
        self.0.to_string()
    }

    pub fn checked_add(self, other: U64Text) -> Result<Self> {
        self.0
            .checked_add(other.0)
            .map(U64Text)
            .ok_or_else(|| Error::Codec(format!("u64 加法溢出：{} + {}", self.0, other.0)))
    }

    /// 快照是进程生命周期内的累计，同一基线键的绝对计数永不下降（C5）。
    /// 因此差分用 checked 减法：**任何一项下降都必须回滚整份快照**，
    /// 不能擅自解释成重启——合法重启会换 `runtime_id`。
    pub fn checked_delta_from(self, previous: U64Text) -> Result<Self> {
        self.0.checked_sub(previous.0).map(U64Text).ok_or_else(|| {
            Error::Codec(format!("计数器倒退：当前 {} < 上次已接受 {}", self.0, previous.0))
        })
    }

    pub fn widen(self) -> U128Text {
        U128Text(self.0 as u128)
    }
}

impl U128Text {
    pub const MAX: U128Text = U128Text(u128::MAX);

    pub const fn new(value: u128) -> Self {
        U128Text(value)
    }

    pub const fn get(self) -> u128 {
        self.0
    }

    pub fn encode(self) -> String {
        format!("{:0width$}", self.0, width = U128_TEXT_WIDTH)
    }

    pub fn decode(text: &str) -> Result<Self> {
        decode_fixed_width(text, U128_TEXT_WIDTH)?
            .parse::<u128>()
            .map(U128Text)
            .map_err(|_| Error::Codec(format!("超出 u128 范围：{text}")))
    }

    pub fn to_api_string(self) -> String {
        self.0.to_string()
    }

    /// 跨方向、跨节点与长期累加。溢出即回滚并告警，不截断。
    pub fn checked_add(self, other: U128Text) -> Result<Self> {
        self.0
            .checked_add(other.0)
            .map(U128Text)
            .ok_or_else(|| Error::Codec(format!("u128 加法溢出：{} + {}", self.0, other.0)))
    }

    /// 把 u128 聚合结果收回 u64 口径（例如把周期累计交给以 u64 为上限的额度计算）。
    pub fn checked_narrow(self) -> Result<U64Text> {
        u64::try_from(self.0)
            .map(U64Text)
            .map_err(|_| Error::Codec(format!("u128 收窄到 u64 溢出：{}", self.0)))
    }
}

fn decode_fixed_width(text: &str, width: usize) -> Result<&str> {
    if text.len() != width {
        return Err(Error::Codec(format!(
            "定宽编码长度必须是 {width}，实际 {}：{text:?}",
            text.len()
        )));
    }
    if !text.bytes().all(|b| b.is_ascii_digit()) {
        return Err(Error::Codec(format!("定宽编码只允许 ASCII 数字：{text:?}")));
    }
    Ok(text)
}

impl std::fmt::Display for U64Text {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.to_api_string())
    }
}

impl std::fmt::Display for U128Text {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.to_api_string())
    }
}

impl From<u64> for U64Text {
    fn from(value: u64) -> Self {
        U64Text(value)
    }
}

impl From<u128> for U128Text {
    fn from(value: u128) -> Self {
        U128Text(value)
    }
}
