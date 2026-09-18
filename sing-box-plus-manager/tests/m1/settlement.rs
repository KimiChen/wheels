//! 结算故障矩阵（README §8）。
//!
//! 前四组对应 README §1 验收定义里的**四项判据**——长跑时它们必须全为 0。
//! 每一条都断言**两件事**：拒绝成因正确，**且状态没有被推进**。
//! 只断言「被拒绝了」不够：一个「拒绝但顺手推了游标」的实现同样能让前半变绿，
//! 而它在生产上表现为静默漏记。

use proxy_manager::ledger::settle::{FirstSnapshot, RejectReason, SettleOutcome};

use super::ledger_harness::{Ledger, Snapshot, NODE, RUNTIME, STARTED_AT_MS};

// ============ 正向：首快照两种策略 ============

/// C8：`baseline` 首次 delta 为 0——把节点已有的累计当作起点。
#[tokio::test]
async fn 首快照baseline不入账但记住游标() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;

    let outcome = ledger.ingest(&Snapshot::new()).await;
    assert!(matches!(outcome, SettleOutcome::Applied { ledger_rows: 0, .. }), "实际：{outcome:?}");
    assert_eq!(ledger.ledger_rows().await, 0, "baseline 首快照不产生账本行");

    // 但游标必须记住绝对值，否则第二份快照会把全部历史当成增量。
    assert_eq!(ledger.cursor("u_example_01").await, Some([100, 200, 0, 0]));
    // **零增量与 active=false 的身份也要更新游标**（§4 第 9 步）。
    assert_eq!(ledger.cursor_count().await, 3, "三个身份都要有游标");
    assert_eq!(ledger.last_sequence().await, Some(1));
}

/// C8：`include` 首次 delta 等于当前累计值。
#[tokio::test]
async fn 首快照include把已有累计一次性入账() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;

    let outcome = ledger.ingest(&Snapshot::new()).await;
    // 样例里 s1 全零（不写行），u_example_01 与 u_example_02 非零。
    assert!(matches!(outcome, SettleOutcome::Applied { ledger_rows: 2, .. }), "实际：{outcome:?}");
    assert_eq!(ledger.lifetime_total("u_example_01").await, 300);
    assert_eq!(ledger.lifetime_total("u_example_02").await, 12);
}

#[tokio::test]
async fn 第二份快照按差值入账() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;

    let second = Snapshot::new().sequence(2).counter("u_example_01", "tcp_uplink_bytes", 1_100);
    let outcome = ledger.ingest(&second).await;
    assert!(matches!(outcome, SettleOutcome::Applied { ledger_rows: 1, .. }), "实际：{outcome:?}");
    assert_eq!(ledger.lifetime_total("u_example_01").await, 1_000, "差值 1100 - 100");
    assert_eq!(ledger.cursor("u_example_01").await, Some([1_100, 200, 0, 0]));
    assert_eq!(ledger.last_sequence().await, Some(2));
}

// ============ 判据一：负增量 ============

/// C5：任何一项下降都**回滚整份快照**——不能局部入账，也不能擅自解释成重启。
#[tokio::test]
async fn 四向任一倒退整份拒绝且不推进任何状态() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;
    let before_cursor = ledger.cursor("u_example_01").await;

    // tcp_downlink 从 200 退到 199，同时另一项在涨——局部入账的实现会在这里露馅。
    let bad = Snapshot::new()
        .sequence(2)
        .counter("u_example_01", "tcp_uplink_bytes", 9_999)
        .counter("u_example_01", "tcp_downlink_bytes", 199);
    let outcome = ledger.ingest(&bad).await;

    match outcome {
        SettleOutcome::Rejected {
            reason: RejectReason::CounterRegression { field, previous, current, .. },
            ..
        } => {
            assert_eq!(field, "tcp_downlink_bytes");
            assert_eq!((previous, current), (200, 199));
        }
        other => panic!("应当因计数器倒退被拒，实际：{other:?}"),
    }
    assert_eq!(ledger.ledger_rows().await, 0, "不得留下任何账本行");
    assert_eq!(ledger.cursor("u_example_01").await, before_cursor, "游标必须原封不动");
    assert_eq!(ledger.last_sequence().await, Some(1), "sequence 不得推进");
}

// ============ 判据二：未知 runtime ============

#[tokio::test]
async fn 未登记的runtime不可入账() {
    let ledger = Ledger::new().await;
    // 刻意不批准。
    let outcome = ledger.ingest(&Snapshot::new()).await;
    assert!(
        matches!(outcome, SettleOutcome::Deferred { reason: RejectReason::UnknownRuntime }),
        "实际：{outcome:?}"
    );
    assert_eq!(ledger.ledger_rows().await, 0);
    assert_eq!(ledger.cursor_count().await, 0, "身份都不该被建出来");
}

/// **正常流程**：先收到快照，再批准 runtime，然后那份快照要能结算。
///
/// `runtime_id` 与 `started_at_unix_ms` 就在快照里，所以运维**不可能**
/// 在看到第一份快照之前批准一个 runtime。把「未批准」判成终态拒绝，
/// 等于让每个新 runtime 的首快照必然丢失一份。
#[tokio::test]
async fn 先收到快照后批准runtime时首快照不丢() {
    let ledger = Ledger::new().await;
    let snapshot = Snapshot::new();

    // 第一次：runtime 还没登记，批次必须**保持 pending**。
    let receipt = ledger.receive(&snapshot).await.expect("回执要留");
    let outcome = ledger.settle().await;
    assert!(matches!(outcome, SettleOutcome::Deferred { .. }), "实际：{outcome:?}");
    assert_eq!(ledger.batch_status(receipt.batch_pk).await, "pending", "不得烧掉这个批次");

    // 运维看到待批准清单之后批准。
    ledger.approve(FirstSnapshot::Include).await;

    // 同一份快照现在必须能结算，而且**一分不少**。
    let outcome = ledger.settle().await;
    assert!(matches!(outcome, SettleOutcome::Applied { ledger_rows: 2, .. }), "实际：{outcome:?}");
    assert_eq!(ledger.lifetime_total("u_example_01").await, 300);
    assert_eq!(ledger.batch_status(receipt.batch_pk).await, "applied");
}

/// 登记了但没批准，同样是可重试而不是终态。
#[tokio::test]
async fn 已登记未批准时也保持pending() {
    let ledger = Ledger::new().await;
    let receipt = ledger.receive(&Snapshot::new()).await.unwrap();

    // 直接插一条 pending 审批的 runtime（不走 approve）。
    let mut txn = ledger.store.begin_immediate().await.unwrap();
    sqlx::query(
        "INSERT INTO node_runtimes(node_id, runtime_id, started_at_unix_ms, first_snapshot, \
         approval_status, first_seen_at, last_seen_at) \
         VALUES (?, ?, ?, 'baseline', 'pending', ?, ?)",
    )
    .bind(NODE)
    .bind(RUNTIME)
    .bind(format!("{:020}", STARTED_AT_MS))
    .bind("2026-09-18T00:00:00Z")
    .bind("2026-09-18T00:00:00Z")
    .execute(txn.conn())
    .await
    .unwrap();
    txn.commit().await.unwrap();

    let outcome = ledger.settle().await;
    assert!(
        matches!(outcome, SettleOutcome::Deferred { reason: RejectReason::RuntimeNotApproved }),
        "实际：{outcome:?}"
    );
    assert_eq!(ledger.batch_status(receipt.batch_pk).await, "pending");
}

/// 反向证据：终态拒绝确实会烧掉批次，不会和上面几条混为一谈。
#[tokio::test]
async fn 终态拒绝与可重试是两回事() {
    use proxy_manager::ledger::settle::RejectReason as R;
    // 主控侧前置条件没满足 —— 可重试。
    assert!(R::UnknownRuntime.is_retryable());
    assert!(R::RuntimeNotApproved.is_retryable());
    // 快照本身不可用 —— 终态。
    assert!(!R::Unhealthy { bits: vec!["counter_overflow"] }.is_retryable());
    assert!(!R::StaleSequence { sequence: 1, last_accepted: 2 }.is_retryable());
    assert!(!R::LineageDisappeared { identities: vec![] }.is_retryable());
    assert!(!R::Malformed("x".into()).is_retryable());

    // 而且终态那条真的把批次烧掉了。
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;
    let receipt = ledger.receive(&Snapshot::new().health("counter_overflow", true)).await.unwrap();
    assert!(matches!(ledger.settle().await, SettleOutcome::Rejected { .. }));
    assert_eq!(ledger.batch_status(receipt.batch_pk).await, "rejected");
}

/// 影子 runtime 的可见形态：收到过快照但没登记。
#[tokio::test]
async fn 待批准清单列出影子runtime() {
    let ledger = Ledger::new().await;
    ledger.receive(&Snapshot::new()).await;
    let pending = proxy_manager::ledger::runtime::awaiting_approval(&ledger.store).await.unwrap();
    assert_eq!(pending.len(), 1);
    assert_eq!(pending[0].1, RUNTIME);

    // 批准之后就不在清单里了。
    ledger.approve(FirstSnapshot::Baseline).await;
    let pending = proxy_manager::ledger::runtime::awaiting_approval(&ledger.store).await.unwrap();
    assert!(pending.is_empty());
}

/// `started_at_unix_ms` 首次确认后固定不变。
#[tokio::test]
async fn 同一runtime报出两个启动时刻会被拒() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    let error = proxy_manager::ledger::runtime::approve(
        &ledger.store,
        NODE,
        RUNTIME,
        STARTED_AT_MS + 1,
        FirstSnapshot::Baseline,
        "再批一次",
        "test",
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("不是同一个进程窗口"), "实际：{error}");
}

/// 批准必须留下原因与操作者——只留策略等于没留。
#[tokio::test]
async fn 批准必须留原因与操作者() {
    let ledger = Ledger::new().await;
    for (reason, actor) in [("", "test"), ("原因", ""), ("   ", "test")] {
        let error = proxy_manager::ledger::runtime::approve(
            &ledger.store,
            NODE,
            RUNTIME,
            STARTED_AT_MS,
            FirstSnapshot::Baseline,
            reason,
            actor,
        )
        .await
        .unwrap_err();
        assert!(error.to_string().contains("原因与操作者"), "实际：{error}");
    }
}

// ============ 判据三：重复 / 回退 sequence ============

#[tokio::test]
async fn sequence回退整份丢弃且不推进任何状态() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new().sequence(5)).await;
    let before = ledger.cursor("u_example_01").await;

    // sequence 4 < 已接受的 5：先被标 superseded（乱序到达）。
    let stale = Snapshot::new().sequence(4).counter("u_example_01", "tcp_uplink_bytes", 9_999);
    let receipt = ledger.receive(&stale).await.expect("回执仍然要留");
    let outcome = ledger.settle().await;
    assert!(
        matches!(outcome, SettleOutcome::Superseded { .. }),
        "更旧且此前未处理的批次标 superseded，实际：{outcome:?}"
    );
    assert_eq!(ledger.batch_status(receipt.batch_pk).await, "superseded");
    assert_eq!(ledger.cursor("u_example_01").await, before, "游标不得变");
    assert_eq!(ledger.last_sequence().await, Some(5));
    assert_eq!(ledger.ledger_rows().await, 0);
}

/// 同号同摘要重投是**幂等**的，不产生第二条回执。
#[tokio::test]
async fn 同号同摘要重投幂等() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    let snapshot = Snapshot::new();

    let first = ledger.receive(&snapshot).await.unwrap();
    assert!(first.freshly_inserted);
    let second = ledger.receive(&snapshot).await.unwrap();
    assert!(!second.freshly_inserted, "同号同摘要不该再插一条");
    assert_eq!(first.batch_pk, second.batch_pk);
}

/// 同号**不同摘要**：节点不该产生这种输入。不改写既有那一行。
#[tokio::test]
async fn 同号不同摘要报错且不改写历史() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    let first = ledger.receive(&Snapshot::new()).await.unwrap();

    let tampered = Snapshot::new().counter("u_example_01", "tcp_uplink_bytes", 999);
    let now = time::OffsetDateTime::now_utc();
    let error = proxy_manager::ledger::settle::receive(
        &ledger.store,
        NODE,
        &tampered.bytes(),
        now,
        now,
        120,
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("摘要不同"), "实际：{error}");
    assert_eq!(ledger.batch_status(first.batch_pk).await, "pending", "既有回执不得被改写");
}

/// C26：**跳号是合法的**。照常按累计差值结算，告警但**绝不对缺失时段插值**。
#[tokio::test]
async fn sequence跳号照常结算差值且不插值() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new().sequence(1)).await;

    // 从 1 直接跳到 10：跳过了 8 个序号。
    let jumped = Snapshot::new().sequence(10).counter("u_example_01", "tcp_uplink_bytes", 900);
    let outcome = ledger.ingest(&jumped).await;
    match outcome {
        SettleOutcome::Applied { sequence_gap, ledger_rows, .. } => {
            assert_eq!(sequence_gap, Some(8), "缺口要如实记录");
            assert_eq!(ledger_rows, 1);
        }
        other => panic!("跳号应当照常入账，实际：{other:?}"),
    }
    // 入账的是**累计差值** 900-100=800，不是按缺口插出来的任何数。
    assert_eq!(ledger.lifetime_total("u_example_01").await, 800);
}

// ============ 判据四：unhealthy 入账 ============

#[tokio::test]
async fn 不可入账的health位为真时整份拒绝() {
    for bit in ["counter_overflow", "sequence_overflow", "identity_limit_reached"] {
        let ledger = Ledger::new().await;
        ledger.approve(FirstSnapshot::Include).await;
        let outcome = ledger.ingest(&Snapshot::new().health(bit, true)).await;
        match outcome {
            SettleOutcome::Rejected { reason: RejectReason::Unhealthy { bits }, .. } => {
                assert!(bits.contains(&bit), "成因要指名是哪一位，实际：{bits:?}");
            }
            other => panic!("{bit} 为真必须整份拒绝，实际：{other:?}"),
        }
        assert_eq!(ledger.ledger_rows().await, 0);
        assert_eq!(ledger.last_sequence().await, None, "不得推进 sequence");
    }
}

/// `audit_dropped` 告警但**照常入账**。
///
/// 把审计丢弃算进入账判据，等于让磁盘写满连带停掉计费——
/// 这是节点侧刻意不做的，主控也不要做回来。
#[tokio::test]
async fn audit_dropped照常入账() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;
    let outcome = ledger.ingest(&Snapshot::new().health("audit_dropped", true)).await;
    assert!(matches!(outcome, SettleOutcome::Applied { ledger_rows: 2, .. }), "实际：{outcome:?}");
    assert_eq!(ledger.lifetime_total("u_example_01").await, 300);
}

// ============ C25 / C9：lineage 的出现与消失 ============

#[tokio::test]
async fn 已观察的lineage消失时整份拒绝() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;
    let before = ledger.cursor("u_example_01").await;

    let missing = Snapshot::new().sequence(2).drop_identity("u_example_02");
    let outcome = ledger.ingest(&missing).await;
    match outcome {
        SettleOutcome::Rejected {
            reason: RejectReason::LineageDisappeared { identities }, ..
        } => {
            assert_eq!(identities, vec!["u_example_02".to_string()]);
        }
        other => panic!("消失的 lineage 必须整份拒绝，实际：{other:?}"),
    }
    assert_eq!(ledger.cursor("u_example_01").await, before);
    assert_eq!(ledger.last_sequence().await, Some(1));
}

/// C2 / C9：`active = false` 的 lineage **继续保留基线**，切回来时复用原累计值。
///
/// 反向证据也在这条里：如果 active=false 被当成消失，上一条用例会误报绿。
#[tokio::test]
async fn active切换不创建新基线也不归零() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;

    // 切成 false：仍在快照里，只是标记变了。必须照常结算。
    let inactive = Snapshot::new().sequence(2).active("u_example_01", false);
    assert!(matches!(ledger.ingest(&inactive).await, SettleOutcome::Applied { .. }));
    assert_eq!(ledger.cursor("u_example_01").await, Some([100, 200, 0, 0]), "基线保留");

    // 切回来并带上新流量：按原累计值差分，不从零开始。
    let back = Snapshot::new().sequence(3).active("u_example_01", true).counter(
        "u_example_01",
        "tcp_uplink_bytes",
        150,
    );
    assert!(matches!(ledger.ingest(&back).await, SettleOutcome::Applied { ledger_rows: 1, .. }));
    assert_eq!(ledger.lifetime_total("u_example_01").await, 50, "150 - 100，不是 150");
}

// ============ u64 边界与跨 u64 聚合 ============

#[tokio::test]
async fn u64边界值精确入账且lifetime跨u64() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;
    ledger.ingest(&Snapshot::new()).await;

    // 四向各推到 u64::MAX：单项是 u64 上界，四向之和超出 u64。
    let huge = Snapshot::new()
        .sequence(2)
        .counter("u_example_01", "tcp_uplink_bytes", u64::MAX)
        .counter("u_example_01", "tcp_downlink_bytes", u64::MAX)
        .counter("u_example_01", "udp_uplink_bytes", u64::MAX)
        .counter("u_example_01", "udp_downlink_bytes", u64::MAX);
    assert!(matches!(ledger.ingest(&huge).await, SettleOutcome::Applied { ledger_rows: 1, .. }));

    // 增量 = MAX-100, MAX-200, MAX, MAX，合计超出 u64，必须在 u128 里精确保存。
    let expected =
        (u64::MAX as u128 - 100) + (u64::MAX as u128 - 200) + u64::MAX as u128 + u64::MAX as u128;
    assert_eq!(ledger.lifetime_total("u_example_01").await, expected);
    assert!(expected > u64::MAX as u128, "这个用例要真的跨过 u64");
    assert_eq!(ledger.cursor("u_example_01").await, Some([u64::MAX; 4]));
}

// ============ 幂等与脏桶 ============

/// 同一批次结算两次不累加。
#[tokio::test]
async fn 重复结算不累加() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;
    ledger.ingest(&Snapshot::new()).await;
    let after_first = ledger.lifetime_total("u_example_01").await;

    // 再结算一次：没有待处理批次了。
    assert!(matches!(ledger.settle().await, SettleOutcome::Nothing));
    assert_eq!(ledger.lifetime_total("u_example_01").await, after_first);
    assert_eq!(ledger.ledger_rows().await, 2);
}

/// 入账要 upsert 对应小时脏桶；同一小时再入账一次，`version` 必须 +1。
#[tokio::test]
async fn 入账会把对应小时标脏且版本递增() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;
    let at = time::macros::datetime!(2026-09-18 10:30:00 UTC);
    ledger.receive_at(&Snapshot::new(), at, at).await;
    ledger.settle().await;
    assert_eq!(ledger.rollup_version("2026-09-18T10:00:00Z").await, Some(1));

    let second = Snapshot::new().sequence(2).counter("u_example_01", "tcp_uplink_bytes", 500);
    let later = time::macros::datetime!(2026-09-18 10:45:00 UTC);
    ledger.receive_at(&second, later, later).await;
    ledger.settle().await;
    assert_eq!(
        ledger.rollup_version("2026-09-18T10:00:00Z").await,
        Some(2),
        "同一桶再脏一次，version 必须 +1——「算的时候又脏了」靠它检测"
    );
}

// ============ 归桶 ============

/// C27：节点时钟偏差超阈值时归桶回落到 `received_at`，且**选择结果落库**。
#[tokio::test]
async fn 时钟偏差超阈值时归桶回落并落库() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Include).await;

    let received = time::macros::datetime!(2026-09-18 10:00:00 UTC);
    let collected = received + time::Duration::seconds(600); // 节点快 10 分钟
    let receipt = ledger.receive_at(&Snapshot::new(), collected, received).await.unwrap();

    let row =
        sqlx::query("SELECT accounting_at, time_quality FROM snapshot_batches WHERE batch_pk = ?")
            .bind(receipt.batch_pk)
            .fetch_one(ledger.store.readers())
            .await
            .unwrap();
    use sqlx::Row;
    assert_eq!(row.get::<String, _>(0), "2026-09-18T10:00:00Z", "回落到 received_at");
    assert_eq!(row.get::<String, _>(1), "fallback");

    // 归桶结果持久化之后，结算必须用**存下来的那个**，否则重放会落进另一个桶。
    ledger.settle().await;
    assert_eq!(ledger.rollup_version("2026-09-18T10:00:00Z").await, Some(1));
}

// ============ 四项判据的正向长跑 ============

/// 一段正常的连续采集之后，四项判据必须全为 0。
///
/// 这条是**正向**用例：上面每一条都在证明「坏输入会被拒」，
/// 而这条证明「好输入不会被误拒」。少了它，一个「拒绝一切」的实现同样能让上面全绿。
#[tokio::test]
async fn 连续正常采集后四项判据全为零() {
    let ledger = Ledger::new().await;
    ledger.approve(FirstSnapshot::Baseline).await;

    let mut expected_total = 0u128;
    for round in 1..=20u64 {
        let up = 100 + round * 1_000;
        let snapshot =
            Snapshot::new().sequence(round).counter("u_example_01", "tcp_uplink_bytes", up);
        let outcome = ledger.ingest(&snapshot).await;
        assert!(
            matches!(outcome, SettleOutcome::Applied { .. }),
            "第 {round} 轮应当正常入账，实际：{outcome:?}"
        );
        if round > 1 {
            expected_total += 1_000;
        }
    }

    assert_eq!(ledger.lifetime_total("u_example_01").await, expected_total);
    assert_eq!(ledger.last_sequence().await, Some(20));

    // 四项判据：账本里不该有任何一条 rejected 回执。
    use sqlx::Row;
    let rejected: i64 = sqlx::query(
        "SELECT count(*) FROM snapshot_batches WHERE status IN ('rejected', 'conflict')",
    )
    .fetch_one(ledger.store.readers())
    .await
    .unwrap()
    .get(0);
    assert_eq!(rejected, 0, "正常长跑不该产生任何拒绝");

    // 不变量：账本合计 == lifetime 合计。
    let ledger_sum: i64 = sqlx::query("SELECT count(*) FROM usage_ledger")
        .fetch_one(ledger.store.readers())
        .await
        .unwrap()
        .get(0);
    assert_eq!(ledger_sum, 19, "首快照 baseline 不写行，之后 19 轮各一行");
}
