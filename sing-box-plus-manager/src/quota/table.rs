//! 完整节点表的组装（C16、C30、C15）。
//!
//! **未出现在 `entries[]` 中的身份 = 无限额度**（C16）。下发的是**限额清单**，
//! 不是用户清单。由此推出主控的义务 C30：每次请求必须枚举该节点
//! **所有受限用户的全部当前有效身份**，包括零额度。
//!
//! 缺一条不是「保持原值」，是**解除限额**——这是这一整层最容易做错的地方，
//! 而且错了之后观测上看不出来：`PUT` 照样返回 200。
//!
//! 反过来的错也要防：已经不在节点当前配置中的身份**不能**继续放进写清单，
//! 否则节点整份 400（C15）。

use std::collections::BTreeMap;

use sqlx::Row;

use crate::error::{Error, Result};
use crate::quota::allocate::{split_identities, NodeWeight};
use crate::quota::wire::Quota;
use crate::store::Store;

/// 节点上一个**当前有效**的计费身份。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NodeIdentity {
    pub runtime_identity_id: i64,
    pub inbound_tag: String,
    pub name: String,
    /// 归属的业务用户。`None` 表示这个身份还没分配给任何人（D11 的账号池）。
    pub user_id: Option<i64>,
}

/// 枚举某 runtime 上的全部当前有效身份。
///
/// **含 `active = false` 的 lineage。** 它们仍在节点的当前配置里——
/// 节点只是把记录切成 `active = false`，并不删除（C9）；
/// 而且按 C25 它们不会从快照里消失。漏掉它们就等于给它们解除限额。
pub async fn current_identities(
    store: &Store,
    node_id: &str,
    runtime_id: &str,
) -> Result<Vec<NodeIdentity>> {
    let rows = sqlx::query(
        "SELECT i.runtime_identity_id, s.inbound_tag, i.identity_name, i.user_id \
         FROM runtime_identities i \
         JOIN runtime_services s ON s.runtime_service_id = i.runtime_service_id \
         JOIN node_runtimes r ON r.runtime_pk = i.runtime_pk \
         WHERE r.node_id = ? AND r.runtime_id = ? \
         ORDER BY s.inbound_tag, i.identity_name",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_all(store.readers())
    .await?;
    Ok(rows
        .into_iter()
        .map(|row| NodeIdentity {
            runtime_identity_id: row.get(0),
            inbound_tag: row.get(1),
            name: row.get(2),
            user_id: row.get(3),
        })
        .collect())
}

/// 组装一份完整节点表。
///
/// `allocations` 是**每个用户在这个节点上**的额度。不在表里的用户按
/// [`Quota::Limited(0)`] 处理而不是 `Unlimited`——安全方向是闸断，不是放行。
///
/// 未分配给任何用户的身份（D11 的账号池）同样是 `Limited(0)`：
/// §4.6 要求身份在全部已批准 runtime 上观察到全零计数后才可分配，
/// 那之前它不该能转发任何字节。**这不是「省略」**——省略等于无限额度。
///
/// 同一用户在同一节点上的多个身份按 D17 情形 3 处理：同口径、按权重切分、
/// **身份级不设地板**。默认全部等权重；口径不同的身份必须由调用方分成
/// 两次调用、各自独立成池（情形 2），那时 `Σ 身份限额 = 节点分配额` 本就不成立。
pub fn assemble(
    identities: &[NodeIdentity],
    allocations: &BTreeMap<i64, Quota>,
) -> Result<Vec<(String, String, Quota)>> {
    // 先按用户分组，才能把一个用户的节点额度切给他的多个身份。
    let mut by_user: BTreeMap<Option<i64>, Vec<&NodeIdentity>> = BTreeMap::new();
    for identity in identities {
        by_user.entry(identity.user_id).or_default().push(identity);
    }

    let mut table = Vec::with_capacity(identities.len());
    for (user_id, group) in by_user {
        let quota = match user_id.and_then(|id| allocations.get(&id).copied()) {
            Some(quota) => quota,
            // 没有归属、或没算出额度 → 零额度。安全方向是闸断。
            None => Quota::Limited(0),
        };

        match quota {
            // 只有**显式** Unlimited 才省略（C30）。
            Quota::Unlimited => {
                for identity in group {
                    table.push((
                        identity.inbound_tag.clone(),
                        identity.name.clone(),
                        Quota::Unlimited,
                    ));
                }
            }
            Quota::Limited(bytes) if group.len() == 1 => {
                let identity = group[0];
                table.push((
                    identity.inbound_tag.clone(),
                    identity.name.clone(),
                    Quota::Limited(bytes),
                ));
            }
            Quota::Limited(bytes) => {
                // 同一用户在同一节点上的多个身份（D17 情形 3）。
                let weights: Vec<NodeWeight> = group
                    .iter()
                    .map(|identity| NodeWeight {
                        node_id: format!("{}\u{1f}{}", identity.inbound_tag, identity.name),
                        weight: 1,
                    })
                    .collect();
                let split = split_identities(bytes, &weights)?;
                for identity in group {
                    let key = format!("{}\u{1f}{}", identity.inbound_tag, identity.name);
                    let share = split.get(&key).copied().ok_or_else(|| {
                        Error::Quota("身份级切分缺少条目：组装逻辑与切分逻辑不一致".into())
                    })?;
                    table.push((
                        identity.inbound_tag.clone(),
                        identity.name.clone(),
                        Quota::Limited(share),
                    ));
                }
            }
        }
    }

    // 逐项唯一。重复身份会让节点侧的 `seen` 去重后 applied 数对不上，
    // 更要紧的是它说明调用方的枚举有 bug。
    let mut seen = std::collections::BTreeSet::new();
    for (tag, name, _) in &table {
        if !seen.insert((tag.clone(), name.clone())) {
            return Err(Error::Quota(format!("完整节点表里身份重复：{tag}/{name}")));
        }
    }
    table.sort_by(|a, b| (&a.0, &a.1).cmp(&(&b.0, &b.1)));
    Ok(table)
}

/// 全零安全表：**把该节点全部受限身份置零**（C33）。
///
/// 注意它和「空表」是两回事。空表同样返回 200，但效果是把所有人**解除限额**。
/// C33 要的是肯定动作：主动下发一份全零完整节点表并拿到 200。
pub fn zero_table(identities: &[NodeIdentity]) -> Vec<(String, String, Quota)> {
    let mut table: Vec<(String, String, Quota)> = identities
        .iter()
        .map(|identity| (identity.inbound_tag.clone(), identity.name.clone(), Quota::Limited(0)))
        .collect();
    table.sort_by(|a, b| (&a.0, &a.1).cmp(&(&b.0, &b.1)));
    table
}

#[cfg(test)]
mod tests {
    use super::*;

    fn identity(tag: &str, name: &str, user: Option<i64>) -> NodeIdentity {
        NodeIdentity {
            runtime_identity_id: 0,
            inbound_tag: tag.into(),
            name: name.into(),
            user_id: user,
        }
    }

    #[test]
    fn 未分配的身份是零额度而不是省略() {
        let identities = vec![identity("in", "pool-slot-1", None)];
        let table = assemble(&identities, &BTreeMap::new()).unwrap();
        assert_eq!(table.len(), 1, "不得省略——省略等于无限额度");
        assert_eq!(table[0].2, Quota::Limited(0));
    }

    #[test]
    fn 算不出额度的用户也是零额度() {
        let identities = vec![identity("in", "u1", Some(7))];
        let table = assemble(&identities, &BTreeMap::new()).unwrap();
        assert_eq!(table[0].2, Quota::Limited(0), "安全方向是闸断，不是放行");
    }

    #[test]
    fn 只有显式unlimited才省略() {
        let identities = vec![identity("in", "u1", Some(1)), identity("in", "u2", Some(2))];
        let allocations = BTreeMap::from([(1i64, Quota::Unlimited), (2i64, Quota::Limited(0))]);
        let table = assemble(&identities, &allocations).unwrap();
        // 组装阶段两条都在；Unlimited 在序列化时才被省略（QuotaRequest::build）。
        assert_eq!(table.len(), 2);
        let request =
            crate::quota::wire::QuotaRequest::build("n", "0".repeat(32), 1, table).unwrap();
        assert_eq!(request.entries.len(), 1, "Unlimited 省略、Limited(0) 保留");
        assert_eq!(request.entries[0].name, "u2");
    }

    #[test]
    fn 同一用户多身份按等权重切分且总和不变() {
        let identities = vec![
            identity("in-a", "u1-a", Some(1)),
            identity("in-b", "u1-b", Some(1)),
            identity("in-c", "u1-c", Some(1)),
        ];
        let allocations = BTreeMap::from([(1i64, Quota::Limited(100))]);
        let table = assemble(&identities, &allocations).unwrap();
        let sum: u64 = table
            .iter()
            .map(|(_, _, q)| match q {
                Quota::Limited(v) => *v,
                Quota::Unlimited => 0,
            })
            .sum();
        assert_eq!(sum, 100, "同口径下 Σ 身份限额 == 节点分配额（D17 情形 3）");
    }

    #[test]
    fn 身份重复会被发现() {
        let identities = vec![identity("in", "u1", Some(1)), identity("in", "u1", Some(2))];
        let allocations = BTreeMap::from([(1i64, Quota::Limited(1)), (2i64, Quota::Limited(1))]);
        assert!(assemble(&identities, &allocations).is_err());
    }

    #[test]
    fn 全零表覆盖每一个身份() {
        let identities = vec![
            identity("in-a", "u1", Some(1)),
            identity("in-b", "u2", None),
            identity("in-b", "u3", Some(3)),
        ];
        let table = zero_table(&identities);
        assert_eq!(table.len(), 3, "全零表不是空表——空表会把所有人解除限额");
        assert!(table.iter().all(|(_, _, q)| *q == Quota::Limited(0)));
    }
}
