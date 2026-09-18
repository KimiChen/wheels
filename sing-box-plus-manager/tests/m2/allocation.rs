//! 分配算法的验收用例（README §8「配额状态机」）。
//!
//! 这一组几乎全是纯函数，所以可以用属性测试压——而属性测试正是这里最合适的形式：
//! 分配算法的失败模式是「某个输入组合下总额对不上或地板被吃掉」，
//! 而那种组合很难靠手写用例枚举出来。

use std::collections::BTreeMap;

use proxy_manager::quota::allocate::{
    plan, split_identities, AllocationConfig, AllocationMode, NodeWeight, WIRE_MAX,
};
use proxy_manager::quota::pool::budget;
use proxy_manager::quota::weight::{self, DemandSample, INITIAL_WEIGHT, MIN_WEIGHT};
use proxy_manager::store::codec::U128Text;

const MIB: u64 = 1024 * 1024;

fn nodes(specs: &[(&str, u32)]) -> Vec<NodeWeight> {
    specs.iter().map(|(id, w)| NodeWeight { node_id: (*id).into(), weight: *w }).collect()
}

fn equal_weights(count: usize) -> Vec<NodeWeight> {
    (0..count)
        .map(|i| NodeWeight { node_id: format!("node-{i:03}"), weight: INITIAL_WEIGHT })
        .collect()
}

/// 确定性 PRNG。不引依赖，也让失败可复现——
/// 一个用随机种子的属性测试，红了之后没法原样再跑一遍。
struct Rng(u64);
impl Rng {
    fn next(&mut self) -> u64 {
        // xorshift64*
        self.0 ^= self.0 >> 12;
        self.0 ^= self.0 << 25;
        self.0 ^= self.0 >> 27;
        self.0.wrapping_mul(0x2545_F491_4F6C_DD1D)
    }
    fn range(&mut self, lo: u64, hi: u64) -> u64 {
        if hi <= lo {
            return lo;
        }
        lo + self.next() % (hi - lo + 1)
    }
}

// ============ §8：无除零 / 无溢出 ============

#[test]
fn n为零或b为零时不产生正额度() {
    let config = AllocationConfig::default();

    // N = 0
    let p = plan(1_000 * MIB, &[], &config, 0).unwrap();
    assert_eq!(p.mode, AllocationMode::NoBudget);
    assert!(p.allocations.is_empty());
    assert_eq!(p.total(), 0);

    // B = 0：每个节点都在表里，但都是 0——**零额度必须显式下发**（C30）。
    let p = plan(0, &equal_weights(3), &config, 0).unwrap();
    assert_eq!(p.mode, AllocationMode::NoBudget);
    assert_eq!(p.allocations.len(), 3, "零额度条目必须在表里，不能省略");
    assert!(p.allocations.values().all(|v| *v == 0));
}

#[test]
fn wire上限截断不破坏总额不变量() {
    let config = AllocationConfig { floor: 1, probe: 1 };
    // 预算远超 int64 上限，单节点会被截断。
    let p = plan(u64::MAX, &equal_weights(1), &config, 0).unwrap();
    assert_eq!(p.get("node-000"), WIRE_MAX, "截到 int64 上限（C31）");
    assert!(p.total() <= u64::MAX as u128, "截断只会让总额变小");
}

#[test]
fn 全部权重相同时均分且无除零() {
    let config = AllocationConfig::default();
    let p = plan(300 * MIB, &equal_weights(3), &config, 0).unwrap();
    assert_eq!(p.total(), 300 * MIB as u128, "总额恰好等于预算");
    let values: Vec<u64> = p.allocations.values().copied().collect();
    let spread = values.iter().max().unwrap() - values.iter().min().unwrap();
    assert!(spread <= 1, "等权重下最多差 1 字节（余数），实际差 {spread}");
}

#[test]
fn 权重之和为零时报错而不是除零() {
    let config = AllocationConfig::default();
    let bad = nodes(&[("a", 0), ("b", 0)]);
    let error = plan(100 * MIB, &bad, &config, 0).unwrap_err();
    assert!(error.to_string().contains("正整数"), "实际：{error}");
}

#[test]
fn 配置本身要自洽() {
    for (floor, probe) in [(64 * MIB, 0u64), (MIB, 64 * MIB)] {
        let config = AllocationConfig { floor, probe };
        assert!(config.validate().is_err(), "floor={floor} probe={probe} 应当被拒");
    }
    assert!(AllocationConfig::default().validate().is_ok());
}

// ============ §8：[64,336] MiB 的那组反例 ============

/// 剩余 300 MiB、两个节点、地板 64 MiB：**两个节点都至少取得 64 MiB**。
///
/// 这条要防的是「最后再按总额归一化一次」——那会把地板按比例缩掉，
/// 而地板的存在理由（用户漫游到冷节点别被立刻闸断）也就没了。
#[test]
fn 剩余三百mib时两个节点都至少拿到地板() {
    let config = AllocationConfig::default(); // floor = 64 MiB
                                              // 权重悬殊：若末尾归一化，弱的那个会被压到地板以下。
    let two = nodes(&[("node-a", 2_000_000), ("node-b", 1)]);
    let p = plan(300 * MIB, &two, &config, 0).unwrap();

    assert_eq!(p.mode, AllocationMode::Floor);
    assert!(!p.floor_defeated);
    for id in ["node-a", "node-b"] {
        assert!(
            p.get(id) >= 64 * MIB,
            "{id} 只拿到 {} 字节，低于地板——地板被最终归一化吃掉了",
            p.get(id)
        );
    }
    assert_eq!(p.total(), 300 * MIB as u128, "总额恰好等于预算，一字节不多不少");
    // 权重大的确实拿得多。
    assert!(p.get("node-a") > p.get("node-b"));
}

/// 最大余数法**结果确定**：换输入顺序不改变结果。
#[test]
fn 最大余数法结果确定() {
    let config = AllocationConfig::default();
    let forward = nodes(&[("node-a", 7), ("node-b", 11), ("node-c", 13)]);
    let reversed = nodes(&[("node-c", 13), ("node-b", 11), ("node-a", 7)]);
    let shuffled = nodes(&[("node-b", 11), ("node-a", 7), ("node-c", 13)]);

    let base = plan(300 * MIB + 7, &forward, &config, 0).unwrap();
    for other in [&reversed, &shuffled] {
        let p = plan(300 * MIB + 7, other, &config, 0).unwrap();
        assert_eq!(p.allocations, base.allocations, "输入顺序不该影响结果");
    }
}

/// 平局按 `node_id` 字节序——没有这条，余数补 1 的归属就不确定。
#[test]
fn 余数平局按node_id字节序() {
    // 地板设成 1 字节：这条用例要验的是余数归属，不是地板。
    let config = AllocationConfig { floor: 1, probe: 1 };
    // 三个等权重节点分 100 字节：33 + 33 + 33，余 1 给字节序最小的。
    let three = nodes(&[("zzz", 1), ("aaa", 1), ("mmm", 1)]);
    let p = plan(100, &three, &config, 0).unwrap();
    assert_eq!(p.get("aaa"), 34, "余数给字节序最小的");
    assert_eq!(p.get("mmm"), 33);
    assert_eq!(p.get("zzz"), 33);
    assert_eq!(p.total(), 100);
}

/// 属性测试：随机分配均满足总额与地板约束。
#[test]
fn 随机分配满足总额与地板约束() {
    let config = AllocationConfig::default();
    let mut rng = Rng(0x5EED_1234_ABCD_0001);

    for round in 0..3_000 {
        let n = rng.range(1, 8) as usize;
        let specs: Vec<NodeWeight> = (0..n)
            .map(|i| NodeWeight {
                node_id: format!("node-{i:03}"),
                // 覆盖 §8 说的 [64, 336] MiB 分配区间对应的权重跨度
                weight: rng.range(1, 2_000_000) as u32,
            })
            .collect();
        // 预算覆盖从 0 到远超 N × floor
        let b = rng.range(0, 1024 * MIB);
        let p = plan(b, &specs, &config, rng.range(0, 16) as usize).unwrap();

        // 1) 总额不超预算（**验收不变量**：新计划分配额之和 ≤ 额度池剩余）
        assert!(p.total() <= b as u128, "round {round}: 总额 {} 超过预算 {b}", p.total());
        // 2) 没有截断时总额恰好等于预算
        if p.allocations.values().all(|v| *v < WIRE_MAX) {
            assert_eq!(p.total(), b as u128, "round {round}: 预算必须分光");
        }
        // 3) 走地板分支时每个节点都不低于地板
        if p.mode == AllocationMode::Floor {
            for (id, value) in &p.allocations {
                assert!(*value >= config.floor, "round {round}: {id} 低于地板");
            }
            assert!(!p.floor_defeated);
        } else if b > 0 {
            assert!(p.floor_defeated, "round {round}: 非地板分支必须标记地板已失效");
        }
        // 4) 每个节点都在表里——零额度也要显式下发（C30）
        assert_eq!(p.allocations.len(), n, "round {round}: 身份不得缺席");
        // 5) 确定性
        let again = plan(b, &specs, &config, 0).unwrap();
        if p.mode != AllocationMode::Rotation {
            assert_eq!(p.allocations, again.allocations, "round {round}: 结果必须确定");
        }
    }
}

// ============ §8：冷节点与探测 ============

/// `B >= N` 时**每个可更新节点都有正额**。
#[test]
fn 预算够每节点一字节时人人有份() {
    let config = AllocationConfig::default();
    for n in [1usize, 2, 5, 17] {
        let specs = equal_weights(n);
        // B 恰好等于 N：最紧的情形
        let p = plan(n as u64, &specs, &config, 0).unwrap();
        assert_eq!(p.mode, AllocationMode::Probe, "N={n}");
        for (id, value) in &p.allocations {
            assert!(*value >= 1, "N={n}: {id} 拿到 0，但 B >= N");
        }
        assert_eq!(p.total(), n as u128);
    }
}

/// `a_i = 0` 的冷节点：不做除法、权重保持为正，仍能拿到探测额度。
#[test]
fn 冷节点不做除法且仍能拿到探测额度() {
    // 连续多轮完全不用，权重降到下界但不为 0。
    let mut w = INITIAL_WEIGHT;
    for _ in 0..64 {
        w = weight::update_weight(w, Ok(DemandSample { allocated: 100, used: 0 })).unwrap();
    }
    assert_eq!(w, MIN_WEIGHT);

    // a_i = 0 的那一轮不做除法，权重不变。
    let held = weight::update_weight(w, Ok(DemandSample { allocated: 0, used: 0 })).unwrap();
    assert_eq!(held, w);

    // 即便权重是下界，热节点权重是上界，冷节点仍然拿得到地板。
    let config = AllocationConfig::default();
    let mixed = nodes(&[("cold", MIN_WEIGHT), ("hot", 2_000_000)]);
    let p = plan(300 * MIB, &mixed, &config, 0).unwrap();
    assert!(p.get("cold") >= config.floor, "冷节点必须拿到地板，否则是零额度吸收态");
}

/// `0 < B < N`：按轮转游标给 B 个节点各 1 字节，**其余为 0**。
#[test]
fn 预算不足每节点一字节时按游标轮转() {
    let config = AllocationConfig::default();
    let specs = equal_weights(5);
    let p = plan(2, &specs, &config, 0).unwrap();

    assert_eq!(p.mode, AllocationMode::Rotation);
    assert!(p.floor_defeated);
    assert_eq!(p.total(), 2, "总额恰好是 B");
    assert_eq!(p.get("node-000"), 1);
    assert_eq!(p.get("node-001"), 1);
    assert_eq!(p.get("node-002"), 0);
    assert_eq!(p.next_rotation_cursor, 2, "建议游标推进到下一位");
}

/// 固定 `0 < B < N`、每轮成功收敛时，**轮转覆盖所有节点**。
///
/// 注意这条**不承诺单轮恢复吞吐**——它只承诺没有节点被永久饿死。
#[test]
fn 轮转最终覆盖所有节点() {
    let config = AllocationConfig::default();
    let n = 7usize;
    let specs = equal_weights(n);
    let b = 2u64; // 固定 0 < B < N

    let mut cursor = 0usize;
    let mut covered: BTreeMap<String, u32> = BTreeMap::new();
    // ceil(7/2) = 4 轮足够覆盖一遍
    for _ in 0..4 {
        let p = plan(b, &specs, &config, cursor).unwrap();
        for (id, value) in &p.allocations {
            if *value > 0 {
                *covered.entry(id.clone()).or_default() += 1;
            }
        }
        // **成功收敛之后才推进游标**
        cursor = p.next_rotation_cursor;
    }
    assert_eq!(covered.len(), n, "四轮之后每个节点都至少拿到过一次：{covered:?}");
}

/// 反向证据：**失败时不推进游标**，否则被跳过的节点会被永久饿死。
#[test]
fn 不推进游标时轮转会原地打转() {
    let config = AllocationConfig::default();
    let specs = equal_weights(7);
    let mut covered: BTreeMap<String, u32> = BTreeMap::new();
    for _ in 0..4 {
        // 刻意每轮都传同一个游标——模拟「下发失败所以没推进」
        let p = plan(2, &specs, &config, 0).unwrap();
        for (id, value) in &p.allocations {
            if *value > 0 {
                *covered.entry(id.clone()).or_default() += 1;
            }
        }
    }
    assert_eq!(covered.len(), 2, "不推进游标就只有头两个节点拿得到——这正是要避免的");
}

// ============ §8：多身份 ============

/// 同节点、**同口径**的多身份：各身份限额之和 **== 该节点分配额**（D17 情形 3）。
#[test]
fn 同口径多身份之和等于节点分配额() {
    let node_alloc = 300 * MIB + 13; // 带余数
    let identities = nodes(&[("id-a", 5), ("id-b", 3), ("id-c", 1)]);
    let split = split_identities(node_alloc, &identities).unwrap();

    let sum: u64 = split.values().sum();
    assert_eq!(sum, node_alloc, "同口径下这条不变量必须精确成立");
    assert_eq!(split.len(), 3);
    // 身份级**不设地板**：权重最小的那个可以很小。
    assert!(split["id-c"] < 64 * MIB, "身份级不该有地板");
}

#[test]
fn 身份级切分也按最大余数法且确定() {
    let identities = nodes(&[("id-z", 1), ("id-a", 1), ("id-m", 1)]);
    let split = split_identities(100, &identities).unwrap();
    assert_eq!(split["id-a"], 34, "余数给字节序最小的");
    assert_eq!(split.values().sum::<u64>(), 100);

    let reordered = nodes(&[("id-m", 1), ("id-z", 1), ("id-a", 1)]);
    assert_eq!(split_identities(100, &reordered).unwrap(), split);
}

/// 不同口径**各自成池**：这条不变量对它们不成立，所以根本不该走同一个 split。
///
/// 这条用例断言的是**建模边界**而不是算法：两个口径各自调用一次，
/// 各自的和等于各自的池，两个池之间没有任何等式。
#[test]
fn 不同口径各自成池() {
    let pool_a = 100 * MIB;
    let pool_b = 40 * MIB;
    let split_a =
        split_identities(pool_a, &nodes(&[("metric-a-1", 1), ("metric-a-2", 1)])).unwrap();
    let split_b = split_identities(pool_b, &nodes(&[("metric-b-1", 1)])).unwrap();

    assert_eq!(split_a.values().sum::<u64>(), pool_a);
    assert_eq!(split_b.values().sum::<u64>(), pool_b);
    // 两个池的字节**不可直接相加**——这里不写任何跨池等式，
    // 写了反而会掩盖「两个口径被错误地加在一起了」这个真问题。
}

// ============ 池与分配串起来 ============

/// 从周期累计一路算到各节点分配额。
#[test]
fn 从周期累计算到各节点分配额() {
    const GIB: u64 = 1 << 30;
    let quota = 300 * GIB;

    // 正常：已用 100 GiB，剩 200 GiB。
    let b = budget(quota, U128Text::new(100 * GIB as u128));
    assert_eq!(b, 200 * GIB);
    let p = plan(b, &equal_weights(2), &AllocationConfig::default(), 0).unwrap();
    assert_eq!(p.total(), b as u128);
    assert_eq!(p.mode, AllocationMode::Floor);

    // 超额：已用 320 GiB > 300 GiB 的额度 → B = 0，全零表。
    let b = budget(quota, U128Text::new(320 * GIB as u128));
    assert_eq!(b, 0, "饱和减法（C32）");
    let p = plan(b, &equal_weights(2), &AllocationConfig::default(), 0).unwrap();
    assert_eq!(p.mode, AllocationMode::NoBudget);
    assert_eq!(p.allocations.len(), 2, "零额度条目仍要在表里（C30）");
    assert!(p.allocations.values().all(|v| *v == 0));
}
