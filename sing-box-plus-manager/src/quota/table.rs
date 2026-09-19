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
    // 归属从**槽位**取，不从 runtime_identities 取：后者按 runtime 键，
    // 节点一重启归属就没了。退役槽位仍保留 user_id（退役不解绑），
    // 所以要把 state 一起读出来，在这里就把它的归属抹掉——它既不该拿到
    // 主人的额度，也不该参与该主人在本节点的份额切分。
    let rows = sqlx::query(
        "SELECT i.runtime_identity_id, s.inbound_tag, i.identity_name, \
                rt.user_id, coalesce(rt.state, 'free') \
         FROM runtime_identities i \
         JOIN runtime_services s ON s.runtime_service_id = i.runtime_service_id \
         JOIN node_runtimes r ON r.runtime_pk = i.runtime_pk \
         LEFT JOIN identity_routes rt \
                ON rt.node_id = r.node_id AND rt.identity_name = i.identity_name \
         WHERE r.node_id = ? AND r.runtime_id = ? \
         ORDER BY s.inbound_tag, i.identity_name",
    )
    .bind(node_id)
    .bind(runtime_id)
    .fetch_all(store.readers())
    .await?;
    Ok(rows
        .into_iter()
        .map(|row| {
            let state: String = row.get(4);
            NodeIdentity {
                runtime_identity_id: row.get(0),
                inbound_tag: row.get(1),
                name: row.get(2),
                user_id: if state == "claimed" { row.get(3) } else { None },
            }
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
/// # 同一个人在一个节点上的多条 inbound 记录：**各发全额，不平分**
///
/// 一个人在一个节点上同时有 SS 与 VLESS 两条入口下的**同名**身份。它们是同一个
/// 计费口径（字节就是字节，四向计数与周期总账都不区分协议），按 D17 情形 3 的
/// 老口径应当平分。这里**刻意不平分**，理由与 D24 拒绝跨节点拆分逐字相同：
/// 平分会让只用 VLESS 的人在**一半额度**处被切断，而那是没人要求过的强制行为，
/// 且在下一轮重新分配之前一直存在。
///
/// ## 代价，量化
///
/// 节点侧的 quotaCell 挂在 `(inbound_tag, name)` 上——`ApplyQuotaTable` 里
/// 每条 entry 解析出一个**独立的** userRecord，节点上**没有任何跨入口的合计**
/// （`sing-box-plus/internal/userstats/quota.go`）。于是一个用户在一台节点上
/// 同时握有两份全额，两份各自倒数。
///
/// **这与「每节点镜像全额」（`converge`、D24）是同一类敞口，不是新的一类。**
/// 三条判据都相同：
///
/// * 成因相同——一份余额被交给 K 个互不通信的执行计数器；
/// * 收敛机制相同——下一轮从账本重算 `剩余 = 额度 − 本周期已用`，而账本把
///   **全部 K 个计数器**的字节收进同一行 `usage_cycle_totals`（`bump_cycle` 按
///   user_id 求和，两条 inbound 的 runtime_identity 经 `slot_owners` 关联到同一个
///   user）。超发的部分下一轮就被扣回去，**不累积**；
/// * 失败方向相同——拿不到新鲜快照的节点在 `converge` 里拿 `Limited(0)`，
///   停住的一侧是闸断而不是放行。
///
/// 变的只有系数：K 从 `节点数` 变成 `节点数 × 承载该名字的入口数`（本轮 4 → 8）。
/// 上界不是 `K × 月度额度`，是
/// `Σ over K cells of min(本周期剩余, 一轮收敛时间 × 该 cell 的线速)`。
///
/// ## 新增的那一条依赖
///
/// 没有新增敞口类别，但「一个人在一个节点上只有一个**名字**」这条从
/// 「少给一半」升级成「再翻一倍敞口」。撑它的是
/// `idx_identity_routes_active_per_user`，而认领的幂等短路让那条索引从来碰不到
/// ——所以现在有一条专门绕过短路、直接写库的用例钉着它。
///
/// `split_identities` 因此不再有在线调用方；模块与用例保留不接线，
/// 留给将来真的需要加权分配的那天。
pub fn assemble(
    identities: &[NodeIdentity],
    allocations: &BTreeMap<i64, Quota>,
) -> Result<Vec<(String, String, Quota)>> {
    // 按用户分组只为取一次额度：同一个人的多条记录拿的是**同一个**值。
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
        // 各发全额。`Unlimited` 也照发——只有**显式** Unlimited 才在序列化时
        // 被省略（C30，见 `QuotaRequest::build`），这里不做那个判断。
        for identity in group {
            table.push((identity.inbound_tag.clone(), identity.name.clone(), quota));
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

    /// 同一个人在一个节点上的多条 inbound 记录：**各拿全额**。
    ///
    /// 这条曾经断言的是相反的行为（`Σ 身份限额 == 节点分配额`，D17 情形 3 的等权切分）。
    /// 改口径的理由与 D24 拒绝跨节点拆分逐字相同：平分会让只用其中一种协议的人
    /// 在**一半额度**处被切断，而那是没人要求过的强制行为。
    ///
    /// 代价写在 `assemble` 的文档里：节点侧 quotaCell 挂在 `(inbound_tag, name)` 上，
    /// 两条入口是两个互不相干的计数器，于是他同时握有两份全额。那与「每节点镜像
    /// 全额」是同一类敞口，系数从节点数变成节点数 × 入口数，收敛靠下一轮从账本重算。
    #[test]
    fn 同一用户在一个节点上的多条入口各拿全额() {
        // 同一个名字、两条入口——这正是 SS + VLESS 的形状。
        let identities =
            vec![identity("ss-in", "u1", Some(1)), identity("vless-in", "u1", Some(1))];
        let allocations = BTreeMap::from([(1i64, Quota::Limited(100))]);
        let table = assemble(&identities, &allocations).unwrap();
        assert_eq!(table.len(), 2);
        assert!(
            table.iter().all(|(_, _, q)| *q == Quota::Limited(100)),
            "各发全额，不平分：{table:?}"
        );
        // 反向：退回平分的话两者各是 50，下面这条会红。
        assert_ne!(table[0].2, Quota::Limited(50));
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
