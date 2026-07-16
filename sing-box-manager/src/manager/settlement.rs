//! 结算屏障 Manager 侧驱动。Agent phase A 回 awaiting_meter_ack 后：GET meter-batch → **单事务** ingest
//! 最终批（entry_id 以 Manager 为权威，精确一次由 traffic_batches PK 保证）→ POST meter-ack（体含
//! boot_id+sequence 作 ingest 证明，Agent 据此才停旧）→ 得 Agent phase B 终态回执。
//! 安全：最终批仅 identity_name+字节/会话数，绝无 uPSK；meter-ack 明文配置全程不涉。

use sqlx::SqlitePool;

use crate::domain::agent::CommandKind;
use crate::domain::deployment::DeployReport;
use crate::domain::metering::MeterAckBody;
use crate::error::Result;
use crate::manager::agent_client::AgentClient;
use crate::store::metering;

/// 一次结算的产出：Agent phase B 终态回执 + 是否真正结算了最终批（供 drive 定 barrier_status）。
pub struct SettleOutcome {
    pub report: DeployReport,
    pub settled: bool,
    pub drain_clean: bool,
}

/// 结算一个返回 awaiting_meter_ack 的 entry 目标。
#[allow(clippy::too_many_arguments)]
pub async fn settle_and_ack(
    pool: &SqlitePool,
    client: &dyn AgentClient,
    entry_id: &str,
    host_id: &str,
    mgmt: &str,
    command_id: &str,
    revision_seq: i64,
    deployment_id: &str,
) -> Result<SettleOutcome> {
    // 1) 取暂存最终批。取批失败/为空 → 不 ack（不授权停旧，约束 1），返回失败终态让 drive 回滚。
    let mb = match client.get_meter_batch(host_id, mgmt, command_id).await {
        Ok(m) => m,
        Err(e) => {
            return Ok(fail_outcome(
                revision_seq,
                format!("取 meter-batch 失败: {e}"),
            ));
        }
    };
    let Some(batch) = mb.batch else {
        return Ok(fail_outcome(
            revision_seq,
            "meter-batch 空，拒绝结算".into(),
        ));
    };

    // 2) 单事务 ingest 最终批（precise-once）。ingest 成功才允许 ack。
    let rd = metering::reset_day(pool).await?;
    metering::ingest_batch(pool, entry_id, &batch, "final", Some(deployment_id), rd).await?;

    // 3) meter-ack（携 ingest 证明）→ Agent phase B 完成切换，回执即终态。
    let ack = MeterAckBody {
        revision: revision_seq,
        singbox_boot_id: batch.singbox_boot_id,
        sequence: batch.sequence,
    };
    let body = serde_json::to_string(&ack).unwrap_or_default();
    let report = match client
        .post_command(host_id, mgmt, CommandKind::MeterAck, command_id, &body)
        .await
    {
        Ok(resp) => serde_json::from_str::<DeployReport>(&resp.body_json)
            .unwrap_or_else(|e| fail(revision_seq, format!("meter-ack 回执解析失败: {e}"))),
        Err(e) => fail(revision_seq, format!("meter-ack 调用失败: {e}")),
    };
    Ok(SettleOutcome {
        report,
        settled: true,
        drain_clean: mb.drain_clean,
    })
}

fn fail(rev: i64, msg: String) -> DeployReport {
    DeployReport {
        status: "failed".into(),
        revision: rev,
        runtime_epoch: None,
        output: Some(msg),
        health: None,
    }
}

fn fail_outcome(rev: i64, msg: String) -> SettleOutcome {
    SettleOutcome {
        report: fail(rev, msg),
        settled: false,
        drain_clean: false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crypto::Cipher;
    use crate::domain::host::Capability;
    use crate::domain::metering::{MeterBatchResponse, StatsBatch, StatsUser};
    use crate::domain::topology::{ExitKind, InboundKind, RouteDraft};
    use crate::manager::agent_client::{AgentResponse, MockAgentClient};
    use crate::store::topology::NewEntry;
    use crate::store::{self, topology as topo};
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn cipher() -> Cipher {
        std::env::set_var("ENCRYPTION_MASTER_KEY", STANDARD.encode([9u8; 32]));
        Cipher::from_env(1).unwrap()
    }

    /// settle_and_ack：ingest 最终批（用量入桶）+ meter-ack（得 phase B 终态）；重投 final 幂等不双计。
    #[tokio::test]
    async fn settle_ingests_final_and_acks_idempotently() {
        let path = std::env::temp_dir().join(format!("sbm-settle-{}.db", uuid::Uuid::new_v4()));
        let pool = store::open(&path.to_string_lossy()).await.unwrap();
        let c = cipher();
        let eh = store::hosts::create_host(&pool, "eh", None, &[Capability::Entry])
            .await
            .unwrap();
        let nh = store::hosts::create_host(&pool, "nh", None, &[Capability::Node])
            .await
            .unwrap();
        store::agents::upsert_agent(&pool, &eh, "127.0.0.1:39736")
            .await
            .unwrap();
        let e1 = topo::create_entry(
            &pool,
            &c,
            &NewEntry {
                host_id: &eh,
                public_address: "e",
                inbound_kind: InboundKind::Shadowsocks,
                ss_method: None,
                allow_direct: false,
            },
        )
        .await
        .unwrap();
        let n1 = topo::create_node(&pool, &c, &nh, "n", true).await.unwrap();
        let r1 = topo::insert_route(
            &pool,
            &RouteDraft {
                id: None,
                label: "r".into(),
                entry_id: e1.clone(),
                hops: vec![],
                exit_kind: ExitKind::Node,
                exit_node_id: Some(n1),
                exit_landing_id: None,
            },
        )
        .await
        .unwrap();
        sqlx::query("UPDATE routes SET status='active' WHERE id=?")
            .bind(&r1)
            .execute(&pool)
            .await
            .unwrap();
        let (uid, _) = store::users::create_user(&pool, "u", 0, "monthly", None)
            .await
            .unwrap();
        let name = store::users::grant_route(&pool, &c, &uid, &r1)
            .await
            .unwrap();
        let rev = store::revisions::compile_and_persist(&pool, &c, &e1, Some("1.13.14"), None)
            .await
            .unwrap();
        let dep = store::deployments::create_deployment(
            &pool, "deploy", &rev.id, None, "normal", "{}", None,
        )
        .await
        .unwrap();

        let final_batch = StatsBatch {
            inbound_tag: "in-shared".into(),
            singbox_boot_id: 1,
            sequence: 0,
            observed_at: crate::store::now_unix(),
            tcp_sessions: 0,
            udp_sessions: 0,
            users: vec![StatsUser {
                identity_name: name.clone(),
                uplink_bytes: 500,
                downlink_bytes: 700,
            }],
        };
        let mock = MockAgentClient::default();
        mock.push_meter_batch(Ok(MeterBatchResponse {
            batch: Some(final_batch.clone()),
            drain_clean: true,
        }));
        // meter-ack 回执：Agent phase B 完成 → deployed。
        mock.push_post(Ok(AgentResponse {
            http_status: 200,
            ok: true,
            body_json: serde_json::json!({"status":"deployed","revision":7,"runtime_epoch":2})
                .to_string(),
            echo_command_id: None,
        }));

        let out = settle_and_ack(&pool, &mock, &e1, &eh, "127.0.0.1:39736", "cmd6", 7, &dep)
            .await
            .unwrap();
        assert!(out.settled);
        assert_eq!(out.report.status, "deployed");
        // 最终批已入用量桶。
        let rd = metering::reset_day(&pool).await.unwrap();
        let period = crate::manager::metering::period::period_for(
            crate::store::now_unix(),
            rd,
            crate::domain::user::ResetCycle::Monthly,
        );
        assert_eq!(
            metering::period_usage(&pool, &uid, &period).await.unwrap(),
            (500, 700)
        );

        // 重投同 final（同 boot/seq）：traffic_batches PK 去重 → 用量不变。
        mock.push_meter_batch(Ok(MeterBatchResponse {
            batch: Some(final_batch),
            drain_clean: true,
        }));
        mock.push_post(Ok(AgentResponse {
            http_status: 200,
            ok: true,
            body_json: serde_json::json!({"status":"deployed","revision":7,"runtime_epoch":2})
                .to_string(),
            echo_command_id: None,
        }));
        settle_and_ack(&pool, &mock, &e1, &eh, "127.0.0.1:39736", "cmd6", 7, &dep)
            .await
            .unwrap();
        assert_eq!(
            metering::period_usage(&pool, &uid, &period).await.unwrap(),
            (500, 700),
            "重投 final 不得双计"
        );
        pool.close().await;
    }
}
