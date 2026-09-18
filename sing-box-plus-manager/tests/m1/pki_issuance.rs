//! 签发阶段的六项校验（`docs/threat-model.md` §2.1）。
//!
//! 这些校验必须在签发阶段完成，因为**运行时不一定有能力再做一次**：
//! 运行时的 verifier 只做精确 leaf 比对与有效期，它看不出
//! 「这张证书的 SAN 本来就不该是这个」。

use std::collections::BTreeSet;

use proxy_manager::pki::{self, issue};
use proxy_manager_wire::verify::Usage;

use super::harness::{self, DNS_NAME, GENERATION, NODE_ID};

/// 1) SAN 精确匹配——**集合相等，不是包含**。
///
/// 「包含」会放过一张多带了一个 SAN 的证书，而多出来的那个正是冒充用的。
#[test]
fn san必须精确匹配而不是包含() {
    let materials = harness::materials();
    let leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    let key_pem = harness::read(&materials.agent_dir.join("server-leaf.key"));
    let used = BTreeSet::new();

    let expected =
        vec![format!("DNS:{DNS_NAME}"), format!("URI:{}{NODE_ID}", issue::NODE_URI_PREFIX)];
    issue::verify_issued(&issue::IssuanceCheck {
        der: &leaf[0],
        key_pem: &key_pem,
        expected_usage: Usage::ServerAuth,
        expected_sans: &expected,
        serial: 1,
        used_serials: &used,
    })
    .expect("真实签发出来的材料必须通过校验");

    // 少一个：不通过。
    let error = issue::verify_issued(&issue::IssuanceCheck {
        der: &leaf[0],
        key_pem: &key_pem,
        expected_usage: Usage::ServerAuth,
        expected_sans: &expected[..1],
        serial: 1,
        used_serials: &used,
    })
    .unwrap_err();
    assert!(error.to_string().contains("SAN"), "实际：{error}");

    // 多一个：同样不通过。这条才是「精确」的意义所在。
    let mut extra = expected.clone();
    extra.push("DNS:evil.example.com".into());
    let error = issue::verify_issued(&issue::IssuanceCheck {
        der: &leaf[0],
        key_pem: &key_pem,
        expected_usage: Usage::ServerAuth,
        expected_sans: &extra,
        serial: 1,
        used_serials: &used,
    })
    .unwrap_err();
    assert!(error.to_string().contains("SAN"), "实际：{error}");
}

/// 2) EKU 单一。一张同时带 serverAuth 与 clientAuth 的证书能在两个方向上冒充。
#[test]
fn eku必须单一() {
    let materials = harness::materials();
    let server_leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    // 真实签发的是 serverAuth，按 clientAuth 校验必须失败。
    let error = proxy_manager_wire::verify::assert_single_eku(&server_leaf[0], Usage::ClientAuth)
        .unwrap_err();
    assert!(error.to_string().contains("clientAuth"), "实际：{error}");
    proxy_manager_wire::verify::assert_single_eku(&server_leaf[0], Usage::ServerAuth)
        .expect("正向必须通过");
}

/// 3) key 与 cert 匹配。
#[test]
fn 私钥与证书必须匹配() {
    let materials = harness::materials();
    let leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    // 拿**另一张**证书的私钥来配：必须失败。
    let wrong_key = harness::read(&materials.controller_dir.join("client-leaf.key"));
    let used = BTreeSet::new();
    let error = issue::verify_issued(&issue::IssuanceCheck {
        der: &leaf[0],
        key_pem: &wrong_key,
        expected_usage: Usage::ServerAuth,
        expected_sans: &[
            format!("DNS:{DNS_NAME}"),
            format!("URI:{}{NODE_ID}", issue::NODE_URI_PREFIX),
        ],
        serial: 1,
        used_serials: &used,
    })
    .unwrap_err();
    assert!(error.to_string().contains("不匹配"), "实际：{error}");
}

/// 4) serial 唯一。
#[test]
fn serial必须唯一() {
    let materials = harness::materials();
    let leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    let key_pem = harness::read(&materials.agent_dir.join("server-leaf.key"));
    let used: BTreeSet<u64> = [7u64].into_iter().collect();
    let error = issue::verify_issued(&issue::IssuanceCheck {
        der: &leaf[0],
        key_pem: &key_pem,
        expected_usage: Usage::ServerAuth,
        expected_sans: &[
            format!("DNS:{DNS_NAME}"),
            format!("URI:{}{NODE_ID}", issue::NODE_URI_PREFIX),
        ],
        serial: 7,
        used_serials: &used,
    })
    .unwrap_err();
    assert!(error.to_string().contains("已被使用过"), "实际：{error}");
}

/// workspace 记录的 serial 在多次签发之间不重复。
#[test]
fn 多次签发的serial互不重复() {
    let materials = harness::materials();
    materials
        .workspace
        .issue_node("node-example-02", GENERATION, "node-02.example.com", &Default::default())
        .unwrap();
    let recorded: Vec<u64> = std::fs::read_to_string(materials.workspace.serials_file())
        .unwrap()
        .lines()
        .filter_map(|line| line.trim().parse::<u64>().ok())
        .collect();
    let unique: BTreeSet<u64> = recorded.iter().copied().collect();
    assert_eq!(recorded.len(), unique.len(), "serial 记录里有重复");
    // root 1 张 + 每个节点 4 张。
    assert_eq!(recorded.len(), 1 + 4 + 4);
}

/// 5) 部署文件集**最小**。多出来的文件往往是私钥。
#[test]
fn 部署文件集必须最小() {
    let materials = harness::materials();
    issue::verify_deployment_set(&materials.agent_dir, issue::AGENT_DEPLOYMENT_FILES)
        .expect("真实签发出来的部署目录必须最小");

    // 手滑把 intermediate 的私钥拷进去：必须被抓住。
    let generation_dir = materials.workspace.generation_dir(NODE_ID, GENERATION);
    std::fs::copy(
        generation_dir.join("server-intermediate.key"),
        materials.agent_dir.join("server-intermediate.key"),
    )
    .unwrap();
    let error = issue::verify_deployment_set(&materials.agent_dir, issue::AGENT_DEPLOYMENT_FILES)
        .unwrap_err();
    assert!(error.to_string().contains("多出"), "实际：{error}");
    assert!(error.to_string().contains("私钥"), "错误要说明为什么这条重要");
}

/// 6) 权限正确：私钥 0600，目录 0700。
#[cfg(unix)]
#[test]
fn 私钥权限必须是0600() {
    use std::os::unix::fs::PermissionsExt;
    let materials = harness::materials();
    let key = materials.agent_dir.join("server-leaf.key");
    issue::verify_permissions(&key).expect("真实签发出来的私钥权限必须正确");
    let mode = std::fs::metadata(&key).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode, 0o600, "实际 {mode:o}");

    // 目录 0700。
    issue::verify_permissions(&materials.agent_dir).expect("部署目录权限必须正确");

    // 改松之后必须被抓住。
    std::fs::set_permissions(&key, std::fs::Permissions::from_mode(0o644)).unwrap();
    let error = issue::verify_permissions(&key).unwrap_err();
    assert!(error.to_string().contains("644"), "实际：{error}");
}

/// root 私钥不覆盖：覆盖等于让所有已签发的材料一起失效，而旧私钥没有第二份。
#[test]
fn 不覆盖既有root私钥() {
    let materials = harness::materials();
    let error = materials.workspace.init(&issue::Validity::default()).unwrap_err();
    assert!(error.to_string().contains("不覆盖"), "实际：{error}");
}

/// 巡检只读公钥叶子，且输出不含 PEM、私钥或证书主题。
#[test]
fn 巡检输出不含敏感片段() {
    use proxy_manager::pki::inspect::{self, Level, Thresholds};
    let materials = harness::materials();
    let paths = vec![
        materials.agent_dir.join("server-leaf.pem"),
        materials.controller_dir.join("client-leaf.pem"),
    ];
    let now = time::OffsetDateTime::now_utc().unix_timestamp();
    let findings = inspect::inspect(&paths, Thresholds::default(), now);
    assert_eq!(findings.len(), 2);
    assert_eq!(inspect::worst(&findings), Level::Ok, "刚签发的证书应当是 Ok");

    let rendered: String = findings
        .iter()
        .map(|f| format!("{} {:?} {}\n", f.path.display(), f.level, f.reason))
        .collect();
    inspect::assert_output_is_safe(&rendered).expect("巡检输出不得含敏感片段");

    // 反向证据：真把 PEM 塞进去会被抓住。
    let pem = harness::read(&materials.agent_dir.join("server-leaf.pem"));
    assert!(inspect::assert_output_is_safe(&pem).is_err());
}

/// 缺失 / 损坏 / 未生效 / 已过期一律 critical，不降级成 warning。
#[test]
fn 巡检的异常一律critical() {
    use proxy_manager::pki::inspect::{self, Level, Thresholds};
    let materials = harness::materials();
    let leaf = materials.agent_dir.join("server-leaf.pem");

    let missing = inspect::inspect(
        &[materials.agent_dir.join("does-not-exist.pem")],
        Thresholds::default(),
        0,
    );
    assert_eq!(missing[0].level, Level::Critical);

    // 已过期：把 now 推到 leaf 有效期之后。
    let far_future = time::OffsetDateTime::now_utc().unix_timestamp() + 86_400 * 3650;
    let expired = inspect::inspect(std::slice::from_ref(&leaf), Thresholds::default(), far_future);
    assert_eq!(expired[0].level, Level::Critical);
    assert_eq!(expired[0].reason, "已过期");

    // 尚未生效。
    let long_past = 0i64;
    let early = inspect::inspect(std::slice::from_ref(&leaf), Thresholds::default(), long_past);
    assert_eq!(early[0].level, Level::Critical);
    assert_eq!(early[0].reason, "尚未生效");

    // 两级阈值：leaf 默认 90 天，推到剩 20 天是 warning，剩 3 天是 critical。
    let now = time::OffsetDateTime::now_utc().unix_timestamp();
    let warning =
        inspect::inspect(std::slice::from_ref(&leaf), Thresholds::default(), now + 86_400 * 70);
    assert_eq!(warning[0].level, Level::Warning);
    let critical = inspect::inspect(&[leaf], Thresholds::default(), now + 86_400 * 88);
    assert_eq!(critical[0].level, Level::Critical);
}
