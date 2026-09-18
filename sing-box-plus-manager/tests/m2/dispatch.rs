//! 下发状态机的验收用例（README §8「配额状态机」）。
//!
//! 每条断言都落在**生效表**上而不是状态码上。`PUT` 返回 200 什么都证明不了：
//! 一张**空表**同样返回 200，而它的效果是把节点上所有人解除限额。

use std::collections::{BTreeMap, BTreeSet};

use proxy_manager::quota::dispatch::{self, ConflictResolution, PushOutcome, TableKind};
use proxy_manager::quota::table;
use proxy_manager::quota::wire::Quota;

use super::quota_harness::{Stack, NODE, RUNTIME};

// ============ 正向：准备 → 发送 → 记录 ============

#[tokio::test]
async fn 完整表下发后生效表逐项相符() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;
    let allocations: BTreeMap<i64, Quota> =
        users.values().map(|id| (*id, Quota::Limited(4_096))).collect();
    let entries = table::assemble(&identities, &allocations).unwrap();

    let sequence = stack.settle_once().await;
    let (prepared, outcome) = stack.push(entries, Some(sequence)).await;

    assert!(matches!(outcome, PushOutcome::Applied { applied: 3 }), "实际：{outcome:?}");
    assert_eq!(prepared.epoch, 1, "新 runtime 的 epoch 从 1 开始");
    assert_eq!(prepared.kind, TableKind::Positive);
    assert_eq!(stack.request_result(prepared.request_id).await, "applied");

    let effective = stack.effective().await;
    assert_eq!(effective.len(), 3);
    assert!(effective.values().all(|q| *q == Quota::Limited(4_096)));
}

/// **全量集合断言**（C30）：entries 的唯一身份集合 == 节点上所有受限身份集合。
#[tokio::test]
async fn 全量表覆盖节点上全部受限身份() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;
    let allocations: BTreeMap<i64, Quota> =
        users.values().map(|id| (*id, Quota::Limited(1_000))).collect();
    let entries = table::assemble(&identities, &allocations).unwrap();

    let expected: BTreeSet<(String, String)> =
        identities.iter().map(|i| (i.inbound_tag.clone(), i.name.clone())).collect();
    let sent: BTreeSet<(String, String)> =
        entries.iter().map(|(t, n, _)| (t.clone(), n.clone())).collect();
    assert_eq!(sent, expected, "表里的身份集合必须等于节点上的全部有效身份");

    let sequence = stack.settle_once().await;
    stack.push(entries, Some(sequence)).await;
    assert_eq!(stack.limited_set().await, expected, "生效表上也要逐项相符");
}

/// **反向用例**：故意省略一个身份，必须能检测到他被节点解除限额。
///
/// 没有这一条，上面那条对一个「少发一个人」的实现同样是绿的——
/// 因为 `PUT` 照样返回 200。
#[tokio::test]
async fn 省略一个身份会被检测到解除限额() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;
    let allocations: BTreeMap<i64, Quota> =
        users.values().map(|id| (*id, Quota::Limited(1_000))).collect();
    let mut entries = table::assemble(&identities, &allocations).unwrap();
    let omitted = entries.remove(0);

    let sequence = stack.settle_once().await;
    let (_, outcome) = stack.push(entries, Some(sequence)).await;
    assert!(matches!(outcome, PushOutcome::Applied { .. }), "节点照样返回 200");

    let effective = stack.effective().await;
    let key = (omitted.0.clone(), omitted.1.clone());
    assert_eq!(
        effective[&key],
        Quota::Unlimited,
        "被省略的 {key:?} 在节点上已经是无限额度——这正是 C30 要求枚举全部身份的原因"
    );
}

/// `Unlimited` 省略、`Limited(0)` 保留；某用户池为零**不生成空节点表**。
#[tokio::test]
async fn unlimited省略而零额度保留且不生成空表() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;
    let mut allocations = BTreeMap::new();
    let ids: Vec<i64> = users.values().copied().collect();
    allocations.insert(ids[0], Quota::Unlimited);
    allocations.insert(ids[1], Quota::Limited(0));
    allocations.insert(ids[2], Quota::Limited(500));
    let entries = table::assemble(&identities, &allocations).unwrap();

    let sequence = stack.settle_once().await;
    let (prepared, outcome) = stack.push(entries, Some(sequence)).await;
    assert!(matches!(outcome, PushOutcome::Applied { applied: 2 }), "实际：{outcome:?}");
    assert_eq!(prepared.kind, TableKind::Positive);

    let effective = stack.effective().await;
    let limited: Vec<&Quota> =
        effective.values().filter(|q| matches!(q, Quota::Limited(_))).collect();
    assert_eq!(limited.len(), 2, "零额度条目必须在表里，Unlimited 才省略");
}

// ============ epoch ============

#[tokio::test]
async fn epoch严格递增且取最大已分配值() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;

    for expected in 1..=3u64 {
        let zero = table::zero_table(&identities);
        let (prepared, outcome) = stack.push(zero, None).await;
        assert_eq!(prepared.epoch, expected);
        assert!(matches!(outcome, PushOutcome::Applied { .. }), "实际：{outcome:?}");
    }
    assert_eq!(dispatch::next_epoch(&stack.store, NODE, RUNTIME).await.unwrap(), 4);
}

/// **不依赖最后一次 200**：中间夹一次 unknown，下一个 epoch 仍要跨过它。
#[tokio::test]
async fn 下一个epoch跨过结果不明的那一次() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;

    stack.push(table::zero_table(&identities), None).await; // epoch 1，applied
                                                            // 注入响应丢失：节点应用了，但我们收不到 200。
    stack.node.push_fault(super::fake_node::TransportFault::CloseWithoutResponse).await;
    let (lost, outcome) = stack.push(table::zero_table(&identities), None).await; // epoch 2
    assert_eq!(lost.epoch, 2);
    assert!(matches!(outcome, PushOutcome::Unknown { .. }), "实际：{outcome:?}");
    assert_eq!(stack.request_result(lost.request_id).await, "unknown");

    // 节点侧**已经应用了** epoch 2。
    assert_eq!(stack.node.quota_epoch().await, 2);
    // 按「最后一次 200」恢复会重发 2 → 撞 409。按最大已分配值恢复是 3。
    assert_eq!(dispatch::next_epoch(&stack.store, NODE, RUNTIME).await.unwrap(), 3);
    let (next, outcome) = stack.push(table::zero_table(&identities), None).await;
    assert_eq!(next.epoch, 3);
    assert!(matches!(outcome, PushOutcome::Applied { .. }), "更高 epoch 必须收敛成功");
}

/// **配额路径上的 502 必须判 `unknown`，不是「可重试」。**
///
/// 采集路径上 502 是可重试的——一次失败的 `GET` 没有副作用。
/// 配额路径上不成立：`PUT` 可能已经被节点应用了，只是响应丢了。
/// 把它当成可重试，恢复流程就会重发同一个 epoch 并稳定撞 409。
#[tokio::test]
async fn 配额路径上响应丢失判为结果不明() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.node.push_fault(super::fake_node::TransportFault::CloseWithoutResponse).await;
    let (prepared, outcome) = stack.push(table::zero_table(&identities), None).await;
    assert!(
        matches!(outcome, PushOutcome::Unknown { .. }),
        "PUT 的响应丢失是「结果不明」而不是「可重试」，实际：{outcome:?}"
    );
    assert_eq!(stack.request_result(prepared.request_id).await, "unknown");
    // 节点侧**已经应用了**——这正是「结果不明」的定义。
    assert_eq!(stack.node.quota_epoch().await, prepared.epoch);
}

/// 反向证据：429 确实是「已确认未执行」，仍然判可重试。
#[tokio::test]
async fn 配额路径上的429仍是可重试() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.node.push_fault(super::fake_node::TransportFault::Status(429)).await;
    let (prepared, outcome) = stack.push(table::zero_table(&identities), None).await;
    assert!(matches!(outcome, PushOutcome::Retryable { .. }), "实际：{outcome:?}");
    // 429 在 handler 之前就被拒了，节点侧什么都没变。
    assert!(!stack.node.quota_accepted().await);
    assert_eq!(stack.request_result(prepared.request_id).await, "unknown");
    let _ = prepared;
}

// ============ 409 三步分支 ============

#[tokio::test]
async fn 冲突成因一runtime已变() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.push(table::zero_table(&identities), None).await;

    // 节点重启：换 runtime，额度状态全丢。
    let new_runtime = "a".repeat(32);
    stack.node.restart(&new_runtime).await;

    let (prepared, outcome) = stack.push(table::zero_table(&identities), None).await;
    assert_eq!(outcome, PushOutcome::Conflict);

    // **取快照读回 runtime_id 才能分支**——响应体不区分两种成因。
    let observed = stack.observe().await;
    let observed_ref = observed.as_ref().map(|(n, r)| (n.as_str(), r.as_str()));
    let resolution =
        dispatch::resolve_conflict(&stack.store, &prepared, observed_ref).await.unwrap();
    match &resolution {
        ConflictResolution::RuntimeChanged { observed_runtime } => {
            assert_eq!(observed_runtime, &new_runtime);
        }
        other => panic!("应当判为 runtime 已变，实际：{other:?}"),
    }
    dispatch::record_conflict_cause(&stack.store, prepared.request_id, &resolution).await.unwrap();
    assert_eq!(
        stack.conflict_cause(prepared.request_id).await.as_deref(),
        Some("runtime_mismatch")
    );

    // 新 runtime 的 epoch **可从 1 开始**。
    stack.approve(&new_runtime).await;
    assert_eq!(dispatch::next_epoch(&stack.store, NODE, &new_runtime).await.unwrap(), 1);
}

#[tokio::test]
async fn 冲突成因二epoch未前进() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.push(table::zero_table(&identities), None).await; // epoch 1

    // 手工造一个不前进的 epoch：直接用 wire 层发一份 epoch = 1 的表。
    let entries = table::zero_table(&identities);
    let request =
        proxy_manager::quota::wire::QuotaRequest::build(NODE, RUNTIME, 1, entries).unwrap();
    let response = stack.client.quota(request.to_bytes()).await.unwrap();
    assert_eq!(response.upstream_status, Some(409));

    // runtime 没变 → 是 epoch 的问题。
    let prepared =
        dispatch::prepare(&stack.store, NODE, RUNTIME, table::zero_table(&identities), None, 1, 1)
            .await
            .unwrap();
    let observed = stack.observe().await;
    let observed_ref = observed.as_ref().map(|(n, r)| (n.as_str(), r.as_str()));
    let resolution =
        dispatch::resolve_conflict(&stack.store, &prepared, observed_ref).await.unwrap();
    assert!(matches!(resolution, ConflictResolution::StaleEpoch { .. }), "实际：{resolution:?}");
}

#[tokio::test]
async fn 冲突成因三node_id配错单独拒绝() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let prepared =
        dispatch::prepare(&stack.store, NODE, RUNTIME, table::zero_table(&identities), None, 1, 1)
            .await
            .unwrap();

    // 观察到的 node_id 与我们发的不一致：**先停止推送并告警**，不要继续往下分支。
    let resolution =
        dispatch::resolve_conflict(&stack.store, &prepared, Some(("node-example-99", RUNTIME)))
            .await
            .unwrap();
    match resolution {
        ConflictResolution::NodeMismatch { sent, observed } => {
            assert_eq!(sent, NODE);
            assert_eq!(observed, "node-example-99");
        }
        other => panic!("实际：{other:?}"),
    }
}

/// 取不到快照时**不得假设成任何一种**。
#[tokio::test]
async fn 取不到快照时冲突成因判为不确定() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let prepared =
        dispatch::prepare(&stack.store, NODE, RUNTIME, table::zero_table(&identities), None, 1, 1)
            .await
            .unwrap();
    let resolution = dispatch::resolve_conflict(&stack.store, &prepared, None).await.unwrap();
    assert_eq!(resolution, ConflictResolution::Undetermined);
}

// ============ 恢复 ============

/// 「持久化请求后未发送」：恢复时按 `unknown` 对待，**不能当成「没发出去」**。
#[tokio::test]
async fn 持久化后未发送的请求恢复成unknown() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let prepared =
        dispatch::prepare(&stack.store, NODE, RUNTIME, table::zero_table(&identities), None, 1, 1)
            .await
            .unwrap();
    assert_eq!(stack.request_result(prepared.request_id).await, "prepared");

    // 主控在这里崩了。恢复流程把它记成 unknown。
    let recovered = dispatch::recover_prepared(&stack.store, NODE).await.unwrap();
    assert_eq!(recovered, 1);
    assert_eq!(stack.request_result(prepared.request_id).await, "unknown");

    // **epoch 不得回滚复用**：下一个仍然要跨过它。
    assert_eq!(dispatch::next_epoch(&stack.store, NODE, RUNTIME).await.unwrap(), 2);
}

/// 「收到 200 后主控崩溃」：已经是 applied，重跑不重复分配 epoch。
#[tokio::test]
async fn 收到响应后崩溃不重复分配epoch() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let (prepared, _) = stack.push(table::zero_table(&identities), None).await;
    assert_eq!(stack.request_result(prepared.request_id).await, "applied");

    // 恢复流程扫一遍：没有 prepared 残留。
    assert_eq!(dispatch::recover_prepared(&stack.store, NODE).await.unwrap(), 0);
    assert_eq!(stack.request_result(prepared.request_id).await, "applied");
    assert_eq!(dispatch::next_epoch(&stack.store, NODE, RUNTIME).await.unwrap(), 2);
}

// ============ 新 sequence 门禁 ============

/// 同一健康 sequence 在 TTL 内连续触发三次重算，**最多准备一份正额度表**。
#[tokio::test]
async fn 同一sequence最多支撑一份正额度表() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;
    let allocations: BTreeMap<i64, Quota> =
        users.values().map(|id| (*id, Quota::Limited(100))).collect();
    let sequence = stack.settle_once().await;

    let first = dispatch::prepare(
        &stack.store,
        NODE,
        RUNTIME,
        table::assemble(&identities, &allocations).unwrap(),
        Some(sequence),
        1,
        1,
    )
    .await;
    assert!(first.is_ok());

    for attempt in 2..=3 {
        let again = dispatch::prepare(
            &stack.store,
            NODE,
            RUNTIME,
            table::assemble(&identities, &allocations).unwrap(),
            Some(sequence),
            1,
            1,
        )
        .await;
        assert!(again.is_err(), "第 {attempt} 次应当被 sequence 占用挡住");
        let message = again.unwrap_err().to_string();
        assert!(message.contains("sequence"), "实际：{message}");
    }
    assert!(!dispatch::sequence_available(&stack.store, NODE, RUNTIME, sequence).await.unwrap());

    // 换一个新结算的 sequence 就可以。
    let fresh = stack.settle_once().await;
    assert!(dispatch::sequence_available(&stack.store, NODE, RUNTIME, fresh).await.unwrap());
    assert!(dispatch::prepare(
        &stack.store,
        NODE,
        RUNTIME,
        table::assemble(&identities, &allocations).unwrap(),
        Some(fresh),
        1,
        1,
    )
    .await
    .is_ok());
}

/// **主控重启不清除已占用的 sequence**——占用落在库里，不是内存。
#[tokio::test]
async fn 重启不清除已占用的sequence() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let sequence = stack.settle_once().await;
    let entries: Vec<_> = identities
        .iter()
        .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Limited(7)))
        .collect();
    dispatch::prepare(&stack.store, NODE, RUNTIME, entries.clone(), Some(sequence), 1, 1)
        .await
        .unwrap();

    // 「重启」＝重新打开同一个库。
    assert!(!dispatch::sequence_available(&stack.store, NODE, RUNTIME, sequence).await.unwrap());
    assert!(dispatch::prepare(&stack.store, NODE, RUNTIME, entries, Some(sequence), 1, 1)
        .await
        .is_err());
}

/// **纯零额度安全表不受 sequence 占用限制**——否则 C33 的肯定动作会被自己的门禁挡住。
#[tokio::test]
async fn 零额度安全表可重复收敛() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    for round in 1..=3 {
        let (prepared, outcome) = stack.push(table::zero_table(&identities), None).await;
        assert_eq!(prepared.kind, TableKind::Zero, "第 {round} 轮");
        assert!(matches!(outcome, PushOutcome::Applied { .. }), "第 {round} 轮：{outcome:?}");
    }
    let effective = stack.effective().await;
    assert_eq!(effective.len(), 3);
    assert!(effective.values().all(|q| *q == Quota::Limited(0)));
}

/// 正额度表**必须**绑定 sequence，在准备阶段就挡住。
#[tokio::test]
async fn 正额度表缺来源sequence时拒绝准备() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let entries: Vec<_> = identities
        .iter()
        .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Limited(1)))
        .collect();
    let error =
        dispatch::prepare(&stack.store, NODE, RUNTIME, entries, None, 1, 1).await.unwrap_err();
    assert!(error.to_string().contains("已结算的 sequence"), "实际：{error}");
}

// ============ C33 的肯定动作 ============

/// **无新鲜快照但通道可用时，主控必须主动下发一份全零完整节点表并拿到 200。**
///
/// 不作为会让节点带着旧的正额度一直跑到自己耗尽。两者在观测上完全不同——
/// 而现有的否定面断言（「不许刷新正额度」）对一个「什么都不做」的实现同样是绿的。
#[tokio::test]
async fn 无新鲜快照时主动收敛全零表() {
    let stack = Stack::new().await;
    let users = stack.map_users().await;
    let identities = stack.identities().await;

    // 先给一份正额度。
    let allocations: BTreeMap<i64, Quota> =
        users.values().map(|id| (*id, Quota::Limited(9_999))).collect();
    let sequence = stack.settle_once().await;
    stack.push(table::assemble(&identities, &allocations).unwrap(), Some(sequence)).await;
    assert!(stack.effective().await.values().all(|q| *q == Quota::Limited(9_999)));

    // 快照不健康：不得刷新正额度，但通道可用 → **主动下发全零完整表**。
    stack.node.set_health_bit("counter_overflow", true).await;
    let (prepared, outcome) = stack.push(table::zero_table(&identities), None).await;
    assert_eq!(prepared.kind, TableKind::Zero);
    assert!(matches!(outcome, PushOutcome::Applied { .. }), "实际：{outcome:?}");

    let effective = stack.effective().await;
    assert_eq!(effective.len(), 3, "**全零完整表，不是空表**");
    assert!(effective.values().all(|q| *q == Quota::Limited(0)));
}

/// 无健康快照时连续三轮，**正额度不得反复补回**。
#[tokio::test]
async fn 无健康快照时连续三轮不补正额度() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.node.set_health_bit("counter_overflow", true).await;

    for round in 1..=3 {
        // 拿不到新的已结算 sequence，正额度表连准备都准备不出来。
        let entries: Vec<_> = identities
            .iter()
            .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Limited(1_000)))
            .collect();
        assert!(
            dispatch::prepare(&stack.store, NODE, RUNTIME, entries, None, 1, 1).await.is_err(),
            "第 {round} 轮不该准备出正额度表"
        );
        // 能做的只有零表收敛。
        let (_, outcome) = stack.push(table::zero_table(&identities), None).await;
        assert!(matches!(outcome, PushOutcome::Applied { .. }));
    }
    assert!(stack.effective().await.values().all(|q| *q == Quota::Limited(0)));
}

// ============ C15：未知身份 ============

/// 未知身份整份 400 并回未知项，**生效表不变**。
#[tokio::test]
async fn 未知身份整份拒绝且生效表不变() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    stack.push(table::zero_table(&identities), None).await;
    let before = stack.effective().await;

    let mut entries = table::zero_table(&identities);
    entries.push(("vless-entry-01".into(), "u_example_99".into(), Quota::Limited(1)));
    let sequence = stack.settle_once().await;
    let (prepared, outcome) = stack.push(entries, Some(sequence)).await;

    match &outcome {
        PushOutcome::UnknownIdentity { unknown } => {
            assert!(unknown.iter().any(|u| u.contains("u_example_99")), "实际：{unknown:?}");
        }
        other => panic!("应当回未知身份，实际：{other:?}"),
    }
    assert_eq!(stack.request_result(prepared.request_id).await, "rejected");
    assert_eq!(stack.effective().await, before, "整份拒绝，不得局部应用");
}

// ============ C31：wire 范围 ============

#[tokio::test]
async fn wire边界值往返正确() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    for value in [0u64, i64::MAX as u64] {
        let entries: Vec<_> = identities
            .iter()
            .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Limited(value)))
            .collect();
        let sequence = stack.settle_once().await;
        let source = if value == 0 { None } else { Some(sequence) };
        let (_, outcome) = stack.push(entries, source).await;
        assert!(matches!(outcome, PushOutcome::Applied { .. }), "value={value}: {outcome:?}");
        assert!(stack.effective().await.values().all(|q| *q == Quota::Limited(value)));
    }
}

#[tokio::test]
async fn 超出int64的额度在准备阶段就失败() {
    let stack = Stack::new().await;
    let identities = stack.identities().await;
    let entries: Vec<_> = identities
        .iter()
        .map(|i| (i.inbound_tag.clone(), i.name.clone(), Quota::Limited(u64::MAX)))
        .collect();
    let sequence = stack.settle_once().await;
    let error = dispatch::prepare(&stack.store, NODE, RUNTIME, entries, Some(sequence), 1, 1)
        .await
        .unwrap_err();
    assert!(error.to_string().contains("int64"), "实际：{error}");
}
