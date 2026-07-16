//! SQLite 存储层：连接池（WAL / 外键 / busy_timeout / synchronous=NORMAL，池上限 4）
//! + 版本化迁移。所有结构变更通过 [`migrations`] 执行。

use crate::error::Result;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous};
use sqlx::SqlitePool;
use std::time::Duration;

pub mod admins;
pub mod agents;
pub mod audit;
pub mod commands;
pub mod deployments;
pub mod hosts;
pub mod metering;
pub mod migrations;
pub mod observability;
pub mod pki;
pub mod reencrypt;
pub mod revisions;
pub mod runtime_state;
pub mod secrets;
pub mod sessions;
pub mod settings;
pub mod snapshot;
pub mod topology;
pub mod users;

/// 当前 UTC Unix 秒。全库时间统一用此函数写入。
pub fn now_unix() -> i64 {
    time::OffsetDateTime::now_utc().unix_timestamp()
}

/// 打开数据库并应用运行参数（WAL/外键/busy_timeout/synchronous），不跑迁移。
/// Manager 库用 [`open`]；Agent 本地库用此 + 自己的迁移集。
pub async fn connect(path: &str) -> Result<SqlitePool> {
    if let Some(dir) = std::path::Path::new(path).parent() {
        if !dir.as_os_str().is_empty() {
            std::fs::create_dir_all(dir).ok();
        }
    }
    let opts = SqliteConnectOptions::new()
        .filename(path)
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .synchronous(SqliteSynchronous::Normal)
        .foreign_keys(true)
        .busy_timeout(Duration::from_secs(5));
    let pool = SqlitePoolOptions::new()
        .max_connections(4)
        .connect_with(opts)
        .await?;
    Ok(pool)
}

/// 打开 Manager 库并跑控制面迁移。
pub async fn open(path: &str) -> Result<SqlitePool> {
    let pool = connect(path).await?;
    migrations::run(&pool).await?;
    Ok(pool)
}

#[cfg(test)]
mod tests {
    use super::*;
    use sqlx::Row;

    #[tokio::test]
    async fn migrations_apply_idempotently_and_enforce_foreign_keys() {
        let path = std::env::temp_dir().join(format!("sbm-store-{}.db", uuid::Uuid::new_v4()));
        let ps = path.to_string_lossy().to_string();

        let pool = open(&ps).await.unwrap();
        let v: i64 = sqlx::query("SELECT COALESCE(MAX(version),0) AS v FROM schema_migrations")
            .fetch_one(&pool)
            .await
            .unwrap()
            .get("v");
        assert_eq!(v, 9);

        // 再次运行迁移应幂等，不重复应用。
        migrations::run(&pool).await.unwrap();

        // 外键生效：为不存在的 host 建 entry 应被拒绝。
        let bad = sqlx::query(
            "INSERT INTO entries(id,host_id,public_address,port,inbound_kind,allow_direct,created_at,updated_at)
             VALUES('e1','nope','a.example.com',19736,'shadowsocks',0,0,0)",
        )
        .execute(&pool)
        .await;
        assert!(bad.is_err(), "外键应拒绝引用不存在的 host");

        // 0002 表与新列可用；agent_commands 外键拒绝不存在的 host。
        let bad_cmd = sqlx::query(
            "INSERT INTO agent_commands(command_id,host_id,kind,idempotency_key,request_hash,request_json,created_at,updated_at)
             VALUES('c1','nope','status','k','h','{}',0,0)",
        )
        .execute(&pool)
        .await;
        assert!(
            bad_cmd.is_err(),
            "外键应拒绝 agent_commands 引用不存在的 host"
        );
        // ALTER 新列存在（查询不应报错）。
        sqlx::query("SELECT singbox_running, consecutive_failures FROM agents LIMIT 0")
            .execute(&pool)
            .await
            .unwrap();
        sqlx::query("SELECT spki_sha256, ca_keypair_id FROM agent_certificates LIMIT 0")
            .execute(&pool)
            .await
            .unwrap();

        // 0003 表可用；config_artifacts 外键拒绝不存在的 revision。
        sqlx::query("SELECT seq, topology_hash FROM config_revisions LIMIT 0")
            .execute(&pool)
            .await
            .unwrap();
        let bad_art = sqlx::query(
            "INSERT INTO config_artifacts(id,revision_id,host_id,role,scope_ref,content_sha256,byte_size,alg,key_version,nonce,ciphertext,generated_at)
             VALUES('a1','nope','nope','entry','e',' ',0,1,1,x'00',x'00',0)",
        )
        .execute(&pool)
        .await;
        assert!(
            bad_art.is_err(),
            "外键应拒绝 config_artifacts 引用不存在的 revision"
        );

        pool.close().await;
        let _ = std::fs::remove_file(&path);
    }
}
