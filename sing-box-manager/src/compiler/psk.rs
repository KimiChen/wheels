//! SS-2022 PSK 生成。密钥长度由方法决定；Node 中继固定用 [`NODE_SS_METHOD`]（16B）以保证
//! Entry 侧中继 outbound 的 method 与目标 Node inbound 的 method/密钥长度一致（SS-2022 握手要求匹配）。

use base64::{engine::general_purpose::STANDARD, Engine};
use rand::RngCore;

/// Node 中继与默认 Entry 入站方法。
pub const NODE_SS_METHOD: &str = "2022-blake3-aes-128-gcm";

/// SS-2022 方法对应的密钥字节数。未知方法回退 16。
pub fn key_len(method: &str) -> usize {
    match method {
        "2022-blake3-aes-128-gcm" => 16,
        "2022-blake3-aes-256-gcm" => 32,
        "2022-blake3-chacha20-poly1305" => 32,
        _ => 16,
    }
}

/// 生成一段 base64(STANDARD) 编码的随机 PSK，长度匹配方法。
pub fn generate_psk(method: &str) -> String {
    let mut buf = vec![0u8; key_len(method)];
    rand::thread_rng().fill_bytes(&mut buf);
    STANDARD.encode(&buf)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn psk_length_matches_method() {
        for (m, n) in [
            ("2022-blake3-aes-128-gcm", 16),
            ("2022-blake3-aes-256-gcm", 32),
            ("2022-blake3-chacha20-poly1305", 32),
        ] {
            let psk = generate_psk(m);
            let raw = STANDARD.decode(&psk).unwrap();
            assert_eq!(raw.len(), n, "method {m}");
        }
        // 随机性：两次不同。
        assert_ne!(generate_psk(NODE_SS_METHOD), generate_psk(NODE_SS_METHOD));
    }
}
