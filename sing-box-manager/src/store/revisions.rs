//! config_revisions / config_artifacts 持久化 + 编译落库编排 + 真实 sing-box check。
//! artifact 明文（含 PSK）经 Cipher 信封加密存储；content_sha256 对明文规范字节计算（单向，可出 API）。

use serde_json::{json, Value};
use sqlx::{Row, SqlitePool};

use crate::compiler::canonical::{canonical_bytes, content_sha256};
use crate::compiler::check::check_config;
use crate::compiler::{self, EntrySnapshot, Terminal};
use crate::crypto::{Cipher, Sealed};
use crate::domain::revision::{ArtifactMeta, ConfigRevision};
use crate::error::{AppError, ErrorCode, Result};
use crate::store::{now_unix, snapshot, topology};

const COMPILER_SCHEMA_VERSION: u32 = 2;

// ---------- revision ----------

fn row_to_revision(r: &sqlx::sqlite::SqliteRow) -> ConfigRevision {
    ConfigRevision {
        id: r.get("id"),
        seq: r.get("seq"),
        status: r.get("status"),
        topology_hash: r.get("topology_hash"),
        summary: r.get("summary"),
        created_at: r.get("created_at"),
    }
}

pub async fn get_revision(pool: &SqlitePool, id: &str) -> Result<Option<ConfigRevision>> {
    let row = sqlx::query(
        "SELECT id,seq,status,topology_hash,summary,created_at FROM config_revisions WHERE id=?",
    )
    .bind(id)
    .fetch_optional(pool)
    .await?;
    Ok(row.as_ref().map(row_to_revision))
}

pub async fn list_revisions(pool: &SqlitePool) -> Result<Vec<ConfigRevision>> {
    let rows = sqlx::query("SELECT id,seq,status,topology_hash,summary,created_at FROM config_revisions ORDER BY seq DESC")
        .fetch_all(pool)
        .await?;
    Ok(rows.iter().map(row_to_revision).collect())
}

/// 幂等：同 topology_hash 复用最新一条，否则新建（seq=MAX+1）。
async fn put_revision(
    pool: &SqlitePool,
    topology_hash: &str,
    summary: &str,
    created_by: Option<&str>,
) -> Result<ConfigRevision> {
    if let Some(row) = sqlx::query(
        "SELECT id,seq,status,topology_hash,summary,created_at FROM config_revisions WHERE topology_hash=? ORDER BY seq DESC LIMIT 1",
    )
    .bind(topology_hash)
    .fetch_optional(pool)
    .await?
    {
        return Ok(row_to_revision(&row));
    }
    let id = uuid::Uuid::new_v4().to_string();
    let now = now_unix();
    let seq: i64 = sqlx::query_scalar("SELECT COALESCE(MAX(seq),0)+1 FROM config_revisions")
        .fetch_one(pool)
        .await?;
    sqlx::query(
        "INSERT INTO config_revisions(id,seq,status,topology_hash,summary,created_by,created_at) VALUES(?,?,'compiled',?,?,?,?)",
    )
    .bind(&id)
    .bind(seq)
    .bind(topology_hash)
    .bind(summary)
    .bind(created_by)
    .bind(now)
    .execute(pool)
    .await?;
    get_revision(pool, &id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "revision 建后不可读"))
}

async fn set_revision_status(pool: &SqlitePool, id: &str, status: &str) -> Result<()> {
    sqlx::query("UPDATE config_revisions SET status=? WHERE id=?")
        .bind(status)
        .bind(id)
        .execute(pool)
        .await?;
    Ok(())
}

// ---------- artifact ----------

/// 一份 artifact 的落库目标（除内容外的定位/元数据）。
struct ArtifactTarget<'a> {
    revision_id: &'a str,
    host_id: &'a str,
    role: &'a str,
    scope_ref: &'a str,
    target_singbox_version: Option<&'a str>,
}

/// 封存一份编译产物（明文 canonical JSON 经信封加密；content_sha256 对明文计算）。幂等到 (revision,role,scope)。
async fn put_artifact(
    pool: &SqlitePool,
    cipher: &Cipher,
    tgt: &ArtifactTarget<'_>,
    content: &Value,
) -> Result<()> {
    let plaintext = canonical_bytes(content);
    let sha = content_sha256(content);
    let sealed = cipher.seal(&plaintext)?;
    let now = now_unix();
    sqlx::query(
        "INSERT INTO config_artifacts(id,revision_id,host_id,role,scope_ref,content_sha256,byte_size,alg,key_version,nonce,ciphertext,target_singbox_version,generated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT(revision_id,role,scope_ref) DO UPDATE SET
            host_id=excluded.host_id, content_sha256=excluded.content_sha256, byte_size=excluded.byte_size,
            alg=excluded.alg, key_version=excluded.key_version, nonce=excluded.nonce, ciphertext=excluded.ciphertext,
            target_singbox_version=excluded.target_singbox_version, generated_at=excluded.generated_at,
            check_status='pending', check_output=NULL, checked_at=NULL",
    )
    .bind(uuid::Uuid::new_v4().to_string())
    .bind(tgt.revision_id)
    .bind(tgt.host_id)
    .bind(tgt.role)
    .bind(tgt.scope_ref)
    .bind(&sha)
    .bind(plaintext.len() as i64)
    .bind(sealed.alg)
    .bind(sealed.key_version)
    .bind(&sealed.nonce)
    .bind(&sealed.ciphertext)
    .bind(tgt.target_singbox_version)
    .bind(now)
    .execute(pool)
    .await?;
    Ok(())
}

fn row_to_artifact_meta(r: &sqlx::sqlite::SqliteRow) -> ArtifactMeta {
    ArtifactMeta {
        id: r.get("id"),
        revision_id: r.get("revision_id"),
        host_id: r.get("host_id"),
        role: r.get("role"),
        scope_ref: r.get("scope_ref"),
        content_sha256: r.get("content_sha256"),
        byte_size: r.get("byte_size"),
        target_singbox_version: r.get("target_singbox_version"),
        check_status: r.get("check_status"),
        check_output: r.get("check_output"),
        generated_at: r.get("generated_at"),
        checked_at: r.get("checked_at"),
    }
}

/// artifact 元数据（无 content / 无密钥）。
pub async fn list_artifact_meta(pool: &SqlitePool, revision_id: &str) -> Result<Vec<ArtifactMeta>> {
    let rows = sqlx::query(
        "SELECT id,revision_id,host_id,role,scope_ref,content_sha256,byte_size,target_singbox_version,check_status,check_output,generated_at,checked_at
         FROM config_artifacts WHERE revision_id=? ORDER BY role, scope_ref",
    )
    .bind(revision_id)
    .fetch_all(pool)
    .await?;
    Ok(rows.iter().map(row_to_artifact_meta).collect())
}

/// 解封某 artifact 明文（仅 check 与 Phase 3 部署使用；绝不出 API）。
pub async fn load_artifact_plaintext(
    pool: &SqlitePool,
    cipher: &Cipher,
    id: &str,
) -> Result<Vec<u8>> {
    let row =
        sqlx::query("SELECT alg,key_version,nonce,ciphertext FROM config_artifacts WHERE id=?")
            .bind(id)
            .fetch_optional(pool)
            .await?
            .ok_or_else(|| AppError::new(ErrorCode::NotFound, "artifact 不存在"))?;
    let sealed = Sealed {
        alg: row.get("alg"),
        key_version: row.get("key_version"),
        nonce: row.get("nonce"),
        ciphertext: row.get("ciphertext"),
    };
    cipher.open(&sealed)
}

// ---------- 编排 ----------

/// 校验通过后编译某 Entry 并落库：建/复用 revision + 逐 host 封存 artifact。返回 revision。
pub async fn compile_and_persist(
    pool: &SqlitePool,
    cipher: &Cipher,
    entry_id: &str,
    target_singbox_version: Option<&str>,
    created_by: Option<&str>,
) -> Result<ConfigRevision> {
    let snap = snapshot::load_entry_snapshot(pool, entry_id).await?;
    let secrets = snapshot::load_secrets(pool, cipher, &snap).await?;
    // Phase 4：注入每 Route 已授权身份（结构态；空集时不折入 hash 以保 Phase 2/3 向后兼容）。
    let identities = crate::store::users::configured_identities(pool, entry_id).await?;
    let topo_hash = content_sha256(&topology_descriptor(&snap, &identities));
    let summary = format!(
        "entry={} routes={} identities={}",
        snap.entry.id,
        snap.routes.len(),
        identities.values().map(|v| v.len()).sum::<usize>(),
    );
    let rev = put_revision(pool, &topo_hash, &summary, created_by).await?;

    let id_map: std::collections::HashMap<String, Vec<String>> = identities.into_iter().collect();
    let compiled = compiler::compile(&snap, &secrets, &id_map)?;
    put_artifact(
        pool,
        cipher,
        &ArtifactTarget {
            revision_id: &rev.id,
            host_id: &snap.entry.host_id,
            role: "entry",
            scope_ref: &snap.entry.id,
            target_singbox_version,
        },
        &compiled.entry,
    )
    .await?;
    for (node_id, cfg) in &compiled.nodes {
        let node = topology::get_node(pool, node_id)
            .await?
            .ok_or_else(|| AppError::new(ErrorCode::NotFound, "node 不存在"))?;
        put_artifact(
            pool,
            cipher,
            &ArtifactTarget {
                revision_id: &rev.id,
                host_id: &node.host_id,
                role: "node",
                scope_ref: node_id,
                target_singbox_version,
            },
            cfg,
        )
        .await?;
    }
    Ok(rev)
}

/// 对某 revision 全部 artifact 跑真实 sing-box check；更新每 artifact 与 revision 状态。
pub async fn run_check(
    pool: &SqlitePool,
    cipher: &Cipher,
    revision_id: &str,
) -> Result<Vec<ArtifactMeta>> {
    let metas = list_artifact_meta(pool, revision_id).await?;
    if metas.is_empty() {
        return Err(AppError::new(ErrorCode::NotFound, "revision 无 artifact"));
    }
    let mut all_passed = true;
    for m in &metas {
        let plaintext = load_artifact_plaintext(pool, cipher, &m.id).await?;
        let redact = extract_secret_values(&plaintext);
        let result = check_config(&plaintext, &redact)?;
        all_passed &= result.passed;
        let now = now_unix();
        sqlx::query(
            "UPDATE config_artifacts SET check_status=?, check_output=?, checked_at=? WHERE id=?",
        )
        .bind(if result.passed { "passed" } else { "failed" })
        .bind(&result.output)
        .bind(now)
        .bind(&m.id)
        .execute(pool)
        .await?;
    }
    set_revision_status(
        pool,
        revision_id,
        if all_passed {
            "checked"
        } else {
            "check_failed"
        },
    )
    .await?;
    list_artifact_meta(pool, revision_id).await
}

/// 键无关的拓扑描述（供 topology_hash；不含任何密钥）。identities 非空才折入，保 Phase 2/3 hash 稳定。
fn topology_descriptor(
    snap: &EntrySnapshot,
    identities: &std::collections::BTreeMap<String, Vec<String>>,
) -> Value {
    let routes: Vec<Value> = snap
        .routes
        .iter()
        .map(|rs| {
            let terminal = match &rs.terminal {
                Terminal::Direct => json!({"kind": "direct"}),
                Terminal::Node(n) => json!({"kind": "node", "node": n.id, "addr": n.data_address}),
                Terminal::Socks5(l) => json!({
                    "kind": "socks5", "landing": l.id, "addr": l.socks5_address,
                    "port": l.socks5_port, "network": l.network
                }),
            };
            json!({
                "label": rs.route.label,
                "exit_kind": rs.route.exit_kind,
                "hops": rs.hops.iter().map(|n| json!({"id": n.id, "addr": n.data_address})).collect::<Vec<_>>(),
                "terminal": terminal,
            })
        })
        .collect();
    let mut desc = json!({
        "compiler_schema_version": COMPILER_SCHEMA_VERSION,
        "entry": {
            "id": snap.entry.id, "host": snap.entry.host_id, "addr": snap.entry.public_address,
            "inbound_kind": snap.entry.inbound_kind, "ss_method": snap.entry.ss_method,
            "allow_direct": snap.entry.allow_direct,
        },
        "routes": routes,
    });
    if !identities.is_empty() {
        desc["identities"] = json!(identities);
    }
    desc
}

/// 从配置 JSON 收集 password/username 值，供 check stderr 脱敏（无需接触 SecretBundle）。
fn extract_secret_values(plaintext: &[u8]) -> Vec<String> {
    let mut out = Vec::new();
    if let Ok(v) = serde_json::from_slice::<Value>(plaintext) {
        collect(&v, &mut out);
    }
    out
}
fn collect(v: &Value, out: &mut Vec<String>) {
    match v {
        Value::Object(m) => {
            for (k, val) in m {
                if (k == "password" || k == "username") && val.is_string() {
                    out.push(val.as_str().unwrap().to_string());
                } else {
                    collect(val, out);
                }
            }
        }
        Value::Array(a) => a.iter().for_each(|e| collect(e, out)),
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::compiler::check;
    use crate::domain::host::Capability;
    use crate::domain::topology::{ExitKind, InboundKind, LandingKind, Network, RouteDraft};
    use crate::store::topology::{NewEntry, NewLanding};
    use crate::store::{self, topology as topo};
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }
    async fn pool() -> SqlitePool {
        let path = std::env::temp_dir().join(format!("sbm-rev-{}.db", uuid::Uuid::new_v4()));
        store::open(&path.to_string_lossy()).await.unwrap()
    }
    fn contains(hay: &[u8], needle: &[u8]) -> bool {
        !needle.is_empty() && hay.windows(needle.len()).any(|w| w == needle)
    }

    #[tokio::test]
    async fn end_to_end_compile_check_persist() {
        let pool = pool().await;
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh1 = store::hosts::create_host(&pool, "nh1", None, &[Capability::Node])
            .await
            .unwrap();
        let nh2 = store::hosts::create_host(&pool, "nh2", None, &[Capability::Node])
            .await
            .unwrap();
        let e1 = topo::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e.example.com",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: true,
            },
        )
        .await
        .unwrap();
        let n1 = topo::create_node(&pool, &c, &nh1, "n1.example.com", true)
            .await
            .unwrap();
        let n2 = topo::create_node(&pool, &c, &nh2, "n2.example.com", true)
            .await
            .unwrap();
        let home = topo::create_landing(
            &pool,
            &c,
            &NewLanding {
                kind: LandingKind::Socks5,
                node_id: None,
                socks5_address: Some("home.example.com"),
                socks5_port: Some(1080),
                network: Network::Both,
                socks_user: Some("u"),
                socks_pass: Some("p"),
            },
        )
        .await
        .unwrap();

        // §14：n1 在 relay1 作终端出口、在 multihop 作中继。
        for d in [
            RouteDraft {
                id: None,
                label: "manage-direct".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::EntryDirect,
                exit_node_id: None,
                exit_landing_id: None,
            },
            RouteDraft {
                id: None,
                label: "relay1".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(n1.clone()),
                exit_landing_id: None,
            },
            RouteDraft {
                id: None,
                label: "multihop".into(),
                entry_id: e1.clone(),
                hops: vec![n1.clone(), n2.clone()],
                exit_kind: ExitKind::Landing,
                exit_node_id: None,
                exit_landing_id: Some(home.clone()),
            },
        ] {
            topo::insert_route(&pool, &d).await.unwrap();
        }

        let rev = compile_and_persist(&pool, &c, &e1, Some("1.13.14"), Some("tester"))
            .await
            .unwrap();
        let metas = list_artifact_meta(&pool, &rev.id).await.unwrap();
        assert_eq!(metas.len(), 3, "entry + n1 + n2");
        assert_eq!(metas.iter().filter(|m| m.role == "node").count(), 2);

        // 确定性：再次编译复用同 revision、同 sha。
        let rev2 = compile_and_persist(&pool, &c, &e1, Some("1.13.14"), Some("tester"))
            .await
            .unwrap();
        assert_eq!(rev.id, rev2.id);
        let metas2 = list_artifact_meta(&pool, &rev.id).await.unwrap();
        for (a, b) in metas.iter().zip(metas2.iter()) {
            assert_eq!(a.content_sha256, b.content_sha256);
        }

        // 密文不含 PSK 明文。
        let entry_psk = store::secrets::open_psk_by_scope(&pool, &c, "entry_psk", &e1)
            .await
            .unwrap()
            .unwrap();
        let ct: Vec<u8> =
            sqlx::query_scalar("SELECT ciphertext FROM config_artifacts WHERE role='entry'")
                .fetch_one(&pool)
                .await
                .unwrap();
        assert!(!contains(&ct, entry_psk.as_bytes()), "密文不得含 PSK 明文");

        // 真实 sing-box check（缺二进制则跳过）。
        if check::available() {
            let checked = run_check(&pool, &c, &rev.id).await.unwrap();
            for m in &checked {
                assert_eq!(
                    m.check_status, "passed",
                    "{} 应过 check: {:?}",
                    m.role, m.check_output
                );
                // check_output 不含 PSK 明文。
                if let Some(o) = &m.check_output {
                    assert!(!o.contains(&entry_psk));
                }
            }
            assert_eq!(
                get_revision(&pool, &rev.id).await.unwrap().unwrap().status,
                "checked"
            );
        }
        pool.close().await;
    }
}
