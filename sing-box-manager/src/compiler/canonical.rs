//! 确定性 JSON 序列化 + sha256。递归按键排序输出 compact 字节，**显式实现**以锁定行为、
//! 抗 serde_json 默认键序变更或未来 `preserve_order` feature 被他处引入（否则历史 sha256 漂移）。
//! 用于 artifact 明文（sealed 前的规范字节）、content_sha256、topology_hash。

use serde_json::Value;

/// 规范化：对象键升序、数组保序、标量用 serde_json 紧凑表示。
pub fn canonical_bytes(v: &Value) -> Vec<u8> {
    let mut out = Vec::new();
    write_value(v, &mut out);
    out
}

fn write_value(v: &Value, out: &mut Vec<u8>) {
    match v {
        Value::Object(map) => {
            out.push(b'{');
            let mut keys: Vec<&String> = map.keys().collect();
            keys.sort_unstable();
            for (i, k) in keys.iter().enumerate() {
                if i > 0 {
                    out.push(b',');
                }
                write_scalar(&Value::String((*k).clone()), out);
                out.push(b':');
                write_value(&map[*k], out);
            }
            out.push(b'}');
        }
        Value::Array(arr) => {
            out.push(b'[');
            for (i, e) in arr.iter().enumerate() {
                if i > 0 {
                    out.push(b',');
                }
                write_value(e, out);
            }
            out.push(b']');
        }
        scalar => write_scalar(scalar, out),
    }
}

fn write_scalar(v: &Value, out: &mut Vec<u8>) {
    // serde_json 对标量（含字符串转义、数字、bool、null）的紧凑输出稳定。
    let bytes = serde_json::to_vec(v).expect("scalar serialize");
    out.extend_from_slice(&bytes);
}

/// 规范字节的 sha256（hex 小写）。
pub fn content_sha256(v: &Value) -> String {
    crate::pki::sha256_hex(&canonical_bytes(v))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn key_order_independent() {
        let a = json!({"b": 1, "a": [3, 2, {"z": 1, "y": 2}]});
        let b = json!({"a": [3, 2, {"y": 2, "z": 1}], "b": 1});
        assert_eq!(canonical_bytes(&a), canonical_bytes(&b));
        assert_eq!(content_sha256(&a), content_sha256(&b));
        // 数组顺序敏感。
        let c = json!({"a": [2, 3, {"z": 1, "y": 2}], "b": 1});
        assert_ne!(canonical_bytes(&a), canonical_bytes(&c));
    }

    #[test]
    fn stable_snapshot() {
        // 锁定编码：若某天该断言失败，说明规范化行为变了，历史 sha256 会漂移——须有意为之。
        let v = json!({"x": 1, "y": "a\"b", "z": [true, null]});
        assert_eq!(
            String::from_utf8(canonical_bytes(&v)).unwrap(),
            r#"{"x":1,"y":"a\"b","z":[true,null]}"#
        );
    }
}
