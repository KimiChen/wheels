//! 快照原文的存储编码（`snapshot_payloads.codec`）。
//!
//! 这张表的 DDL 从第一天起就写着「**压缩后的原始 JSON**」，
//! 而且 `CHECK(codec IN ('identity', 'zstd'))` 早就留好了位置——
//! 只是写入侧一直硬编码成 `'identity'`。代价在生产上量出来了：
//! 四台节点、60 秒一轮、每份约 41.7 KB，**每月约 7 GB**，
//! 而其中 301 个身份的计数器绝大多数是零，重复度极高。
//! 实测一份样本 41,676 字节 → 压缩后 1,298 字节，**32 倍**。
//!
//! **压的是同一串字节，不是重新序列化的对象。** 哈希（`snapshot_batches.payload_sha256`）
//! 始终对解压后的原文计算，所以压缩对审计链完全透明：
//! 「用原始载荷做字节守恒核对」这件事一个字节都没损失。
//! 这也是为什么这里选压缩而不是保留期删除——
//! 删除会把那条核对的可用窗口砍成 N 天，而它是**证明计量在工作的唯一办法**。

use crate::error::{Error, Result};

pub const IDENTITY: &str = "identity";
pub const ZSTD: &str = "zstd";

/// zstd 等级。**故意取默认的 3 而不是更高**：
/// 编码发生在采集回路里，而写入侧是单写者（D8）——
/// 省下来的那几个字节不值得让所有节点的入账排在一次压缩后面。
/// 这种数据在 3 级就已经到 30 倍以上，再往上是几个百分点的事。
const LEVEL: i32 = 3;

/// 把原文编成落库的形态。
///
/// **压不小就退回 `identity`。** 极短的输入压完会更大，
/// 那时存压缩结果既费空间又多一道解码。
pub fn encode(raw: &[u8]) -> (&'static str, Vec<u8>) {
    match zstd::encode_all(raw, LEVEL) {
        Ok(packed) if packed.len() < raw.len() => (ZSTD, packed),
        Ok(_) => (IDENTITY, raw.to_vec()),
        Err(error) => {
            // 压缩失败不该让一次入账失败：原文照存，账照记。
            tracing::warn!(%error, "载荷压缩失败，按 identity 原样存储");
            (IDENTITY, raw.to_vec())
        }
    }
}

/// 从库里取回原文。
///
/// **两道都要过**：codec 必须认识，解出来的长度必须等于 `raw_length`。
/// 不认识的 codec **不能当成原文放行**——那会把一串压缩字节喂给 JSON 解析器，
/// 得到的是「畸形快照」这个**看起来很合理但完全错误**的结论，
/// 而真相是这个二进制读不懂自己库里的数据。
pub fn decode(codec: &str, stored: &[u8], raw_length: i64) -> Result<Vec<u8>> {
    let raw = match codec {
        IDENTITY => stored.to_vec(),
        ZSTD => zstd::decode_all(stored)
            .map_err(|e| Error::Ledger(format!("载荷 zstd 解压失败：{e}")))?,
        other => {
            return Err(Error::Ledger(format!(
                "载荷 codec {other:?} 不认识：这个二进制读不懂自己库里的数据，拒绝按原文处理"
            )))
        }
    };
    if raw.len() as i64 != raw_length {
        return Err(Error::Ledger(format!(
            "载荷长度对不上：raw_length={raw_length}，解出来 {}",
            raw.len()
        )));
    }
    Ok(raw)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 往返必须逐字节相等——哈希是对原文算的，差一个字节整条审计链就断了。
    #[test]
    fn 压缩往返逐字节相等() {
        let raw = br#"{"schema_version":3,"identities":[]}"#.repeat(200);
        let (codec, stored) = encode(&raw);
        assert_eq!(codec, ZSTD, "这种重复数据必须压得下去");
        assert!(stored.len() * 4 < raw.len(), "压缩比太差：{} -> {}", raw.len(), stored.len());
        assert_eq!(decode(codec, &stored, raw.len() as i64).unwrap(), raw);
    }

    /// 压不小就退回 identity，而不是存一个更大的东西。
    #[test]
    fn 压不小就原样存() {
        let raw = b"x";
        let (codec, stored) = encode(raw);
        assert_eq!(codec, IDENTITY);
        assert_eq!(stored, raw);
        assert_eq!(decode(codec, &stored, 1).unwrap(), raw);
    }

    /// **不认识的 codec 必须失败关闭。**
    ///
    /// 放行的后果是把压缩字节喂给 JSON 解析器，然后得到「畸形快照」——
    /// 一个看起来很合理、指向节点的结论，而真相在主控这边。
    #[test]
    fn 不认识的codec失败关闭() {
        let error = decode("brotli", b"whatever", 8).unwrap_err().to_string();
        assert!(error.contains("brotli"), "要说清是哪个 codec：{error}");
        assert!(error.contains("读不懂"), "{error}");
    }

    /// 长度对不上也要失败关闭：那说明库里那一行被改过，或者编码口径变了。
    #[test]
    fn 长度对不上失败关闭() {
        let (codec, stored) = encode(b"hello world");
        let error = decode(codec, &stored, 999).unwrap_err().to_string();
        assert!(error.contains("长度对不上"), "{error}");
    }

    /// 旧行是 identity，新二进制必须照样读得出来——
    /// 这条是「不迁移」的前提：两种 codec 长期共存。
    #[test]
    fn 新旧codec共存() {
        let raw = b"{\"schema_version\":3}";
        assert_eq!(decode(IDENTITY, raw, raw.len() as i64).unwrap(), raw);
        let (codec, stored) = encode(&raw.repeat(100));
        assert_eq!(codec, ZSTD);
        assert!(decode(ZSTD, &stored, (raw.len() * 100) as i64).is_ok());
    }
}
