//! 主密钥轮换 re-encrypt 扫描器。把库内全部信封密文逐条 open(旧版本)→seal(当前版本)，
//! 覆盖三张含 `(alg,key_version,nonce,ciphertext)` 的表：credential_versions / ca_keypairs / config_artifacts。
//!
//! **幂等 + 可续跑**：过滤 `WHERE key_version<>current` 天然跳过已迁移行；每批小事务，崩溃后从剩余旧行续跑；
//! `UPDATE ... WHERE <pk>=? AND key_version=<旧>` 守卫使并发/重复处理成为 no-op。明文只在内存 open→seal。
//! **退休门禁**：三表 `pending_counts` 全 0 才允许从 env 删除旧 `ENCRYPTION_MASTER_KEY_V{old}`。

use serde::Serialize;
use sqlx::{Row, SqlitePool};

use crate::crypto::{Cipher, Sealed};
use crate::error::Result;
use crate::store::now_unix;

/// 一张含信封列的表及其主键列。
struct SealedTable {
    name: &'static str,
    pk_cols: &'static [&'static str],
}

const TABLES: &[SealedTable] = &[
    SealedTable {
        name: "credential_versions",
        pk_cols: &["credential_id", "version"],
    },
    SealedTable {
        name: "ca_keypairs",
        pk_cols: &["id"],
    },
    SealedTable {
        name: "config_artifacts",
        pk_cols: &["id"],
    },
];

const DEFAULT_BATCH: i64 = 200;

/// 各表待迁移（key_version != current）行数。
#[derive(Debug, Clone, Serialize)]
pub struct PendingCount {
    pub table: String,
    pub pending: i64,
}

pub async fn pending_counts(pool: &SqlitePool, current: i64) -> Result<Vec<PendingCount>> {
    let mut out = Vec::new();
    for t in TABLES {
        let sql = format!("SELECT COUNT(*) FROM {} WHERE key_version<>?", t.name);
        let n: i64 = sqlx::query_scalar(&sql)
            .bind(current)
            .fetch_one(pool)
            .await?;
        out.push(PendingCount {
            table: t.name.to_string(),
            pending: n,
        });
    }
    Ok(out)
}

/// 是否可安全退休（无任何旧版本密文）。
pub async fn all_migrated(pool: &SqlitePool, current: i64) -> Result<bool> {
    Ok(pending_counts(pool, current)
        .await?
        .iter()
        .all(|p| p.pending == 0))
}

#[derive(Debug, Clone, Serialize, Default)]
pub struct ReencryptReport {
    pub per_table: Vec<(String, u64)>,
    pub total: u64,
}

/// 把三张表全部旧密文 re-seal 到 `cipher.current_version()`。可重复调用（幂等）。
pub async fn reseal_all(
    pool: &SqlitePool,
    cipher: &Cipher,
    batch: Option<i64>,
) -> Result<ReencryptReport> {
    let batch = batch.unwrap_or(DEFAULT_BATCH).clamp(1, 5000);
    let mut report = ReencryptReport::default();
    for t in TABLES {
        let n = reseal_table(pool, cipher, t, batch).await?;
        report.per_table.push((t.name.to_string(), n));
        report.total += n;
    }
    Ok(report)
}

async fn reseal_table(
    pool: &SqlitePool,
    cipher: &Cipher,
    t: &SealedTable,
    batch: i64,
) -> Result<u64> {
    let current = cipher.current_version();
    let pk_list = t.pk_cols.join(",");
    let select_sql = format!(
        "SELECT {pk_list}, alg, key_version, nonce, ciphertext FROM {} WHERE key_version<>? ORDER BY {pk_list} LIMIT ?",
        t.name
    );
    let where_pk: String = t
        .pk_cols
        .iter()
        .map(|c| format!("{c}=?"))
        .collect::<Vec<_>>()
        .join(" AND ");
    let update_sql = format!(
        "UPDATE {} SET alg=?, key_version=?, nonce=?, ciphertext=? WHERE {where_pk} AND key_version=?",
        t.name
    );

    // 记进度：running。
    mark_progress(pool, current, t.name, "running", None).await?;
    let mut migrated = 0u64;
    loop {
        let rows = sqlx::query(&select_sql)
            .bind(current)
            .bind(batch)
            .fetch_all(pool)
            .await?;
        if rows.is_empty() {
            break;
        }
        let mut tx = pool.begin().await?;
        for r in &rows {
            let sealed = Sealed {
                alg: r.get("alg"),
                key_version: r.get("key_version"),
                nonce: r.get::<Vec<u8>, _>("nonce"),
                ciphertext: r.get::<Vec<u8>, _>("ciphertext"),
            };
            let Some(resealed) = cipher.reseal(&sealed)? else {
                continue; // 已是 current（并发下）
            };
            let mut q = sqlx::query(&update_sql)
                .bind(resealed.alg)
                .bind(resealed.key_version)
                .bind(resealed.nonce)
                .bind(resealed.ciphertext);
            // 绑定各 PK 值（按 pk_cols 顺序，类型统一取字符串/整数——用原始 SqliteValue 重绑）。
            for c in t.pk_cols {
                q = bind_pk(q, r, c);
            }
            q = q.bind(sealed.key_version); // 守卫：仍是旧版本才写
            q.execute(&mut *tx).await?;
            migrated += 1;
        }
        tx.commit().await?;
        if rows.len() < batch as usize {
            break;
        }
    }
    mark_progress(pool, current, t.name, "done", Some(migrated as i64)).await?;
    Ok(migrated)
}

/// 按列名把 PK 值从行重绑到 query（credential_versions.version 是整数，其余为文本）。
fn bind_pk<'q>(
    q: sqlx::query::Query<'q, sqlx::Sqlite, sqlx::sqlite::SqliteArguments<'q>>,
    row: &sqlx::sqlite::SqliteRow,
    col: &str,
) -> sqlx::query::Query<'q, sqlx::Sqlite, sqlx::sqlite::SqliteArguments<'q>> {
    if col == "version" {
        q.bind(row.get::<i64, _>(col))
    } else {
        q.bind(row.get::<String, _>(col))
    }
}

async fn mark_progress(
    pool: &SqlitePool,
    target: i64,
    table: &str,
    status: &str,
    rows_done: Option<i64>,
) -> Result<()> {
    let now = now_unix();
    let total: i64 = {
        let sql = format!("SELECT COUNT(*) FROM {table}");
        sqlx::query_scalar(&sql).fetch_one(pool).await?
    };
    sqlx::query(
        "INSERT INTO key_rotation_progress(target_version,table_name,rows_total,rows_done,status,started_at,updated_at,finished_at)
         VALUES(?,?,?,?,?,?,?,?)
         ON CONFLICT(target_version,table_name) DO UPDATE SET
            rows_total=excluded.rows_total,
            rows_done=COALESCE(excluded.rows_done, key_rotation_progress.rows_done),
            status=excluded.status, updated_at=excluded.updated_at,
            started_at=COALESCE(key_rotation_progress.started_at, excluded.started_at),
            finished_at=excluded.finished_at",
    )
    .bind(target)
    .bind(table)
    .bind(total)
    .bind(rows_done.unwrap_or(0))
    .bind(status)
    .bind(now)
    .bind(now)
    .bind(if status == "done" { Some(now) } else { None })
    .execute(pool)
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store;
    use std::collections::HashMap;

    // 用两版本 ring 构造 current=2、含历史 v1 的 Cipher（复用 crypto 内部同款）。
    fn ring_v2() -> Cipher {
        std::env::set_var(
            "ENCRYPTION_MASTER_KEY_V1",
            base64::Engine::encode(&base64::engine::general_purpose::STANDARD, [1u8; 32]),
        );
        std::env::set_var(
            "ENCRYPTION_MASTER_KEY",
            base64::Engine::encode(&base64::engine::general_purpose::STANDARD, [2u8; 32]),
        );
        std::env::set_var("ENCRYPTION_MASTER_KEY_VERSION", "2");
        let c = Cipher::from_env_ring().unwrap();
        std::env::remove_var("ENCRYPTION_MASTER_KEY_V1");
        std::env::remove_var("ENCRYPTION_MASTER_KEY_VERSION");
        c
    }
    fn v1() -> Cipher {
        Cipher::from_raw(1, &[1u8; 32]).unwrap()
    }

    async fn insert_sealed(pool: &SqlitePool, table: &str, pk: &[(&str, &str)], s: &Sealed) {
        let cols: Vec<&str> = pk.iter().map(|(k, _)| *k).collect();
        let sql = format!(
            "INSERT INTO {table}({}, alg, key_version, nonce, ciphertext, created_at) VALUES({}, ?, ?, ?, ?, 0)",
            cols.join(","),
            vec!["?"; cols.len()].join(",")
        );
        let mut q = sqlx::query(&sql);
        for (_, v) in pk {
            q = q.bind(*v);
        }
        q.bind(s.alg)
            .bind(s.key_version)
            .bind(s.nonce.clone())
            .bind(s.ciphertext.clone())
            .execute(pool)
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn reseal_migrates_all_tables_idempotently_and_preserves_plaintext() {
        let path = std::env::temp_dir().join(format!("sbm-reenc-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let ring = ring_v2();
        let old = v1();

        // 满足外键的最小父行。信封列不依赖父行内容。
        let mut plains: HashMap<String, Vec<u8>> = HashMap::new();
        let hid =
            store::hosts::create_host(&pool, "h", None, &[crate::domain::host::Capability::Entry])
                .await
                .unwrap();

        // credentials 父行 + credential_versions v1 密文。
        sqlx::query("INSERT INTO credentials(id,kind,scope,created_at) VALUES('c1','user_route_upsk','s',0)")
            .execute(&pool).await.unwrap();
        let s = old.seal(b"upsk-secret").unwrap();
        plains.insert("cred".into(), b"upsk-secret".to_vec());
        insert_sealed(
            &pool,
            "credential_versions",
            &[("credential_id", "c1"), ("version", "1")],
            &s,
        )
        .await;

        // ca_keypairs v1。
        let s = old.seal(b"ca-priv-pem").unwrap();
        sqlx::query("INSERT INTO ca_keypairs(id,role,cert_pem,spki_sha256,alg,key_version,nonce,ciphertext,not_before,not_after,created_at) VALUES('k1','agent_ca','pem','sha',?,?,?,?,0,0,0)")
            .bind(s.alg).bind(s.key_version).bind(s.nonce.clone()).bind(s.ciphertext.clone())
            .execute(&pool).await.unwrap();

        // config_revisions 父行 + config_artifacts v1。
        sqlx::query("INSERT INTO config_revisions(id,seq,status,topology_hash,created_at) VALUES('r1',1,'compiled','hash',0)")
            .execute(&pool).await.unwrap();
        let s = old.seal(b"config-json").unwrap();
        sqlx::query("INSERT INTO config_artifacts(id,revision_id,host_id,role,scope_ref,content_sha256,byte_size,alg,key_version,nonce,ciphertext,generated_at) VALUES('a1','r1',?,'entry','e1','sha',0,?,?,?,?,0)")
            .bind(&hid).bind(s.alg).bind(s.key_version).bind(s.nonce.clone()).bind(s.ciphertext.clone())
            .execute(&pool).await.unwrap();

        // 迁移前：三表各 1 待迁移。
        let pc = pending_counts(&pool, 2).await.unwrap();
        assert_eq!(pc.iter().map(|p| p.pending).sum::<i64>(), 3);
        assert!(!all_migrated(&pool, 2).await.unwrap());

        // reseal 全部。
        let rep = reseal_all(&pool, &ring, Some(10)).await.unwrap();
        assert_eq!(rep.total, 3);
        assert!(all_migrated(&pool, 2).await.unwrap());

        // 明文未变、版本变 2、可用 ring 解。
        let row = sqlx::query("SELECT alg,key_version,nonce,ciphertext FROM credential_versions WHERE credential_id='c1' AND version=1")
            .fetch_one(&pool).await.unwrap();
        let sealed = Sealed {
            alg: row.get("alg"),
            key_version: row.get("key_version"),
            nonce: row.get("nonce"),
            ciphertext: row.get("ciphertext"),
        };
        assert_eq!(sealed.key_version, 2);
        assert_eq!(ring.open(&sealed).unwrap(), plains["cred"]);

        // 幂等：再跑 0 行。
        let rep2 = reseal_all(&pool, &ring, Some(10)).await.unwrap();
        assert_eq!(rep2.total, 0);
        pool.close().await;
    }
}
