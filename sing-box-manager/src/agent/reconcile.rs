//! Agent 侧 reconcile：把期望身份集下发本机 SSM。uPSK 仅内存 + mTLS，绝不落日志/审计；回执只含名字/计数。

use std::collections::BTreeMap;

use crate::agent::ssm::SsmClient;
use crate::domain::user::{ReconcilePush, ReconcileReport};
use crate::error::Result;

pub async fn execute_reconcile(
    ssm: &dyn SsmClient,
    push: &ReconcilePush,
) -> Result<ReconcileReport> {
    let desired: BTreeMap<String, String> = push
        .users
        .iter()
        .map(|u| (u.name.clone(), u.upsk.clone()))
        .collect();
    ssm.reconcile(&push.inbound_tag, &desired).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agent::ssm::MockSsmClient;
    use crate::domain::user::ReconcileUser;

    fn push(names: &[&str]) -> ReconcilePush {
        ReconcilePush {
            inbound_tag: "in-shared".into(),
            users: names
                .iter()
                .map(|n| ReconcileUser {
                    name: (*n).into(),
                    upsk: format!("psk-{n}"),
                })
                .collect(),
        }
    }

    #[tokio::test]
    async fn reconcile_adds_and_removes_to_match_desired() {
        let ssm = MockSsmClient::default();
        // 初次：加 a,b。
        let r = execute_reconcile(&ssm, &push(&["a", "b"])).await.unwrap();
        assert_eq!(r.added, vec!["a", "b"]);
        assert_eq!(ssm.names(), vec!["a", "b"]);
        // 变更为 b,c：加 c 删 a。
        let r2 = execute_reconcile(&ssm, &push(&["b", "c"])).await.unwrap();
        assert_eq!(r2.added, vec!["c"]);
        assert_eq!(r2.removed, vec!["a"]);
        assert_eq!(ssm.names(), vec!["b", "c"]);
    }

    #[tokio::test]
    async fn reconcile_refills_after_restart_empty_ssm() {
        // 模拟 Entry 重启后 SSM 空：reconcile 按期望态全量 re-add（fresh command_id 场景的核心）。
        let ssm = MockSsmClient::default();
        let r = execute_reconcile(&ssm, &push(&["x", "y", "z"]))
            .await
            .unwrap();
        assert_eq!(r.added.len(), 3);
        assert_eq!(ssm.names(), vec!["x", "y", "z"]);
    }
}
