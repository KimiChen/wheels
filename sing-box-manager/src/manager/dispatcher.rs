//! 命令调度：领取→派发→超时/退避重试→结果；超时先向 Agent 对账（GET-then-repost）再决定重试或超时。
//! 全程注入 [`Clock`] 便于确定性测试。持久化先于派发 + Agent execute_once 幂等 → 重投无重复副作用。

use sha2::{Digest, Sha256};
use sqlx::SqlitePool;

use crate::domain::agent::{CommandKind, CommandStatus};
use crate::error::{AppError, ErrorCode, Result};
use crate::manager::agent_client::{AgentClient, AgentError, AgentResponse};
use crate::manager::clock::Clock;
use crate::store;
use crate::store::commands::{CommandResult, CommandRow, NewCommand};

const MAX_ATTEMPTS: i64 = 3;
const TIMEOUT_MS: i64 = 15000;

/// 创建（或取回）一条命令，返回权威 command_id。请求体哈希用于同键异体检测（409）。
pub async fn enqueue(
    pool: &SqlitePool,
    host_id: &str,
    kind: CommandKind,
    idempotency_key: &str,
    body_json: &str,
) -> Result<String> {
    let cmd_id = uuid::Uuid::new_v4().to_string();
    let request_hash = sha256_hex(body_json);
    let nc = NewCommand {
        host_id,
        kind,
        idempotency_key,
        request_hash: &request_hash,
        request_json: body_json,
        max_attempts: MAX_ATTEMPTS,
        timeout_ms: TIMEOUT_MS,
    };
    store::commands::create_or_get(pool, &cmd_id, &nc).await
}

enum Disposition {
    Succeeded(CommandResult),
    Failed(CommandResult),
    Retry(CommandResult),
}

/// 派发一条命令（须为 pending）。已被领取/未到 not_before 则跳过。
pub async fn dispatch_once(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    clock: &dyn Clock,
    cmd: &CommandRow,
    mgmt_address: &str,
) -> Result<()> {
    if cmd.not_before > clock.now_unix() {
        return Ok(());
    }
    if !store::commands::claim_for_dispatch(pool, &cmd.command_id).await? {
        return Ok(());
    }
    let attempts_after = cmd.attempts + 1;
    let kind = CommandKind::parse(&cmd.kind)
        .ok_or_else(|| AppError::new(ErrorCode::Internal, "未知命令类型"))?;

    let disp = match client
        .post_command(
            &cmd.host_id,
            mgmt_address,
            kind,
            &cmd.command_id,
            &cmd.request_json,
        )
        .await
    {
        Ok(resp) if resp.ok => Disposition::Succeeded(to_result(&resp)),
        Ok(resp) if resp.http_status >= 500 => Disposition::Retry(to_result(&resp)),
        Ok(resp) => Disposition::Failed(to_result(&resp)), // 4xx：校验类，不重试
        Err(AgentError::Timeout) | Err(AgentError::Connect) => {
            // 对账：Agent 是否已知该命令（把超时当非终态）。
            match client
                .get_deployment(&cmd.host_id, mgmt_address, &cmd.command_id)
                .await
            {
                Ok(Some(resp)) if resp.ok => Disposition::Succeeded(to_result(&resp)),
                Ok(Some(resp)) => Disposition::Failed(to_result(&resp)),
                _ => Disposition::Retry(err_result("timeout", "agent 不可达")),
            }
        }
        Err(AgentError::Http(code)) if code >= 500 => {
            Disposition::Retry(err_result("http", &code.to_string()))
        }
        Err(AgentError::Http(code)) => Disposition::Failed(err_result("http", &code.to_string())),
        Err(AgentError::Tls) => Disposition::Failed(err_result("tls", "mTLS 校验失败")),
        Err(e) => Disposition::Retry(err_result("transport", &e.to_string())),
    };

    finalize(pool, clock, cmd, attempts_after, disp).await
}

/// 对账一条崩溃遗留的 in_flight 命令：Agent 已知则回填终态，否则改回 pending 待重投。
pub async fn reconcile_in_flight(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    clock: &dyn Clock,
    cmd: &CommandRow,
    mgmt_address: &str,
) -> Result<()> {
    match client
        .get_deployment(&cmd.host_id, mgmt_address, &cmd.command_id)
        .await
    {
        Ok(Some(resp)) if resp.ok => {
            store::commands::complete(
                pool,
                &cmd.command_id,
                CommandStatus::Succeeded,
                &to_result(&resp),
            )
            .await
        }
        Ok(Some(resp)) => {
            store::commands::complete(
                pool,
                &cmd.command_id,
                CommandStatus::Failed,
                &to_result(&resp),
            )
            .await
        }
        _ => store::commands::requeue(pool, &cmd.command_id, clock.now_unix()).await,
    }
}

/// 遍历未终结命令：pending 派发；in_flight（崩溃遗留）对账。
pub async fn dispatch_pending(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    clock: &dyn Clock,
) -> Result<()> {
    for cmd in store::commands::list_recoverable(pool).await? {
        let Some(agent) = store::agents::get_agent(pool, &cmd.host_id).await? else {
            continue;
        };
        let r = if cmd.status == "in_flight" {
            reconcile_in_flight(pool, client, clock, &cmd, &agent.mgmt_address).await
        } else {
            dispatch_once(pool, client, clock, &cmd, &agent.mgmt_address).await
        };
        if let Err(e) = r {
            tracing::warn!(command = %cmd.command_id, error = %e, "命令处理失败");
        }
    }
    Ok(())
}

async fn finalize(
    pool: &SqlitePool,
    clock: &dyn Clock,
    cmd: &CommandRow,
    attempts_after: i64,
    disp: Disposition,
) -> Result<()> {
    match disp {
        Disposition::Succeeded(r) => {
            store::commands::complete(pool, &cmd.command_id, CommandStatus::Succeeded, &r).await
        }
        Disposition::Failed(r) => {
            store::commands::complete(pool, &cmd.command_id, CommandStatus::Failed, &r).await
        }
        Disposition::Retry(giveup) => {
            if attempts_after < cmd.max_attempts {
                let nb = clock.now_unix() + backoff_secs(attempts_after);
                store::commands::requeue(pool, &cmd.command_id, nb).await
            } else {
                store::commands::complete(pool, &cmd.command_id, CommandStatus::TimedOut, &giveup)
                    .await
            }
        }
    }
}

fn backoff_secs(attempts_after: i64) -> i64 {
    let e = attempts_after.clamp(1, 6) as u32;
    (1i64 << e).min(60)
}

fn to_result(resp: &AgentResponse) -> CommandResult {
    CommandResult {
        ok: resp.ok,
        http_status: Some(resp.http_status as i64),
        result_json: Some(resp.body_json.clone()),
        agent_echo_command_id: resp.echo_command_id.clone(),
        error_code: None,
        error_message: None,
    }
}

fn err_result(code: &str, msg: &str) -> CommandResult {
    CommandResult {
        ok: false,
        http_status: None,
        result_json: None,
        agent_echo_command_id: None,
        error_code: Some(code.into()),
        error_message: Some(msg.into()),
    }
}

pub fn sha256_hex(s: &str) -> String {
    let mut h = Sha256::new();
    h.update(s.as_bytes());
    let d = h.finalize();
    use std::fmt::Write;
    let mut out = String::with_capacity(d.len() * 2);
    for b in d {
        let _ = write!(out, "{b:02x}");
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::host::Capability;
    use crate::manager::agent_client::MockAgentClient;
    use crate::manager::clock::TestClock;

    async fn setup() -> (sqlx::SqlitePool, String) {
        let path = std::env::temp_dir().join(format!("sbm-disp-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let host = store::hosts::create_host(&pool, "h", None, &[Capability::Entry])
            .await
            .unwrap();
        (pool, host)
    }

    fn ok_resp() -> AgentResponse {
        AgentResponse {
            http_status: 200,
            ok: true,
            body_json: "{}".into(),
            echo_command_id: None,
        }
    }

    async fn fetch(pool: &sqlx::SqlitePool, id: &str) -> CommandRow {
        store::commands::fetch_command(pool, id)
            .await
            .unwrap()
            .unwrap()
    }

    #[tokio::test]
    async fn success_marks_succeeded() {
        let (pool, host) = setup().await;
        let clock = TestClock::new(1000);
        let id = enqueue(&pool, &host, CommandKind::Reconcile, "k1", "{}")
            .await
            .unwrap();
        let mock = MockAgentClient::default();
        mock.push_post(Ok(ok_resp()));
        dispatch_once(
            &pool,
            &mock,
            &clock,
            &fetch(&pool, &id).await,
            "127.0.0.1:39736",
        )
        .await
        .unwrap();
        assert_eq!(fetch(&pool, &id).await.status, "succeeded");
        pool.close().await;
    }

    #[tokio::test]
    async fn timeout_unknown_requeues_with_backoff() {
        let (pool, host) = setup().await;
        let clock = TestClock::new(1000);
        let id = enqueue(&pool, &host, CommandKind::Reconcile, "k1", "{}")
            .await
            .unwrap();
        let mock = MockAgentClient::default();
        mock.push_post(Err(AgentError::Timeout));
        mock.push_deployment(Ok(None)); // agent 不知道
        dispatch_once(
            &pool,
            &mock,
            &clock,
            &fetch(&pool, &id).await,
            "127.0.0.1:39736",
        )
        .await
        .unwrap();
        let cmd = fetch(&pool, &id).await;
        assert_eq!(cmd.status, "pending");
        assert!(cmd.not_before > 1000, "应设退避 not_before");
        pool.close().await;
    }

    #[tokio::test]
    async fn timeout_exhausted_times_out() {
        let (pool, host) = setup().await;
        let clock = TestClock::new(1000);
        // max_attempts=1：首次尝试即耗尽。
        let cmd_id = uuid::Uuid::new_v4().to_string();
        store::commands::create_or_get(
            &pool,
            &cmd_id,
            &NewCommand {
                host_id: &host,
                kind: CommandKind::Reconcile,
                idempotency_key: "k",
                request_hash: "h",
                request_json: "{}",
                max_attempts: 1,
                timeout_ms: TIMEOUT_MS,
            },
        )
        .await
        .unwrap();
        let mock = MockAgentClient::default();
        mock.push_post(Err(AgentError::Timeout));
        mock.push_deployment(Ok(None));
        dispatch_once(&pool, &mock, &clock, &fetch(&pool, &cmd_id).await, "x")
            .await
            .unwrap();
        assert_eq!(fetch(&pool, &cmd_id).await.status, "timed_out");
        pool.close().await;
    }

    #[tokio::test]
    async fn http_4xx_fails_without_retry() {
        let (pool, host) = setup().await;
        let clock = TestClock::new(1000);
        let id = enqueue(&pool, &host, CommandKind::Reconcile, "k1", "{}")
            .await
            .unwrap();
        let mock = MockAgentClient::default();
        mock.push_post(Err(AgentError::Http(400)));
        dispatch_once(&pool, &mock, &clock, &fetch(&pool, &id).await, "x")
            .await
            .unwrap();
        assert_eq!(fetch(&pool, &id).await.status, "failed");
        pool.close().await;
    }

    #[tokio::test]
    async fn timeout_but_agent_knows_reconciles_succeeded() {
        let (pool, host) = setup().await;
        let clock = TestClock::new(1000);
        let id = enqueue(&pool, &host, CommandKind::Reconcile, "k1", "{}")
            .await
            .unwrap();
        let mock = MockAgentClient::default();
        mock.push_post(Err(AgentError::Timeout));
        mock.push_deployment(Ok(Some(ok_resp()))); // agent 其实已处理
        dispatch_once(&pool, &mock, &clock, &fetch(&pool, &id).await, "x")
            .await
            .unwrap();
        assert_eq!(fetch(&pool, &id).await.status, "succeeded");
        pool.close().await;
    }
}
