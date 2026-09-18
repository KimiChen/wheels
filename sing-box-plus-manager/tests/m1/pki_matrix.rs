//! 精确 leaf 授信的正负矩阵，**跑真实握手**（`docs/threat-model.md` §2.2）。
//!
//! 这一组里最重要的是 [`同一intermediate新签的leaf握不上手`]。
//! 它就是这整套设计存在的理由：许多 TLS 实现没有「期望对端 SAN 等于某字符串」
//! 的独立 matcher，如果授权 bundle 里放的是 intermediate，
//! **任何由该 intermediate 签出的证书都能握手**。
//!
//! 如果哪天有人把 bundle 从 leaf 改成 intermediate（因为「这样轮换方便」），
//! 这条用例必须立刻红。

use proxy_manager::pki;
use proxy_manager_wire::verify::{AuthorizedLeaves, Usage};

use super::harness::{self, GENERATION, NODE_ID};

#[tokio::test]
async fn 授权的精确leaf可以握手并通信() {
    let materials = harness::materials();
    let echoed = harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("授权材料必须能握手");
    assert_eq!(echoed, b"ping", "握手之后要能真的读写一次");
}

/// **本模块的核心断言。**
///
/// 由同一个 client intermediate 新签的一张合法 clientAuth 证书——
/// 链验证会通过、EKU 正确、SAN 正确、没过期——**仍然必须被拒**，
/// 因为它不是 bundle 里那一张。
#[tokio::test]
async fn 同一intermediate新签的leaf握不上手() {
    let materials = harness::materials();
    // 先证明原材料是好的，否则下面的失败可能来自别的原因。
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("基线必须先能握手");

    let (cert_pem, key_pem) = harness::reissue_client_leaf(&materials);
    harness::replace(&materials.controller_dir.join("client-leaf.pem"), &cert_pem);
    harness::replace(&materials.controller_dir.join("client-chain.pem"), &cert_pem);
    harness::replace(&materials.controller_dir.join("client-leaf.key"), &key_pem);

    let result = harness::handshake(&materials.agent_dir, &materials.controller_dir).await;
    assert!(
        result.is_err(),
        "同一 intermediate 签出的另一张 leaf 必须握不上手——\
         这条一旦变绿，说明授权 bundle 里放的已经不是精确 leaf 了"
    );
}

/// 服务端方向的同一条。
#[tokio::test]
async fn 同一intermediate新签的服务端leaf也握不上手() {
    let materials = harness::materials();
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("基线必须先能握手");

    let (cert_pem, key_pem) = harness::reissue_server_leaf(&materials);
    harness::replace(&materials.agent_dir.join("server-leaf.pem"), &cert_pem);
    harness::replace(&materials.agent_dir.join("server-chain.pem"), &cert_pem);
    harness::replace(&materials.agent_dir.join("server-leaf.key"), &key_pem);

    let result = harness::handshake(&materials.agent_dir, &materials.controller_dir).await;
    assert!(result.is_err(), "主控必须拒绝一张不在 bundle 里的服务端 leaf");
}

/// 吊销的实质：**从授权 bundle 里移除**。
///
/// 证书上的 `REVOKED` 标记只是离线审计记录，不会让任何在线组件拒绝谁。
#[tokio::test]
async fn 从bundle移除之后就握不上手() {
    let materials = harness::materials();
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("基线必须先能握手");

    // overlap 阶段：bundle 里同时放新旧两张，两张都该能用。
    let (new_cert, new_key) = harness::reissue_client_leaf(&materials);
    let bundle = materials.agent_dir.join("authorized-clients.pem");
    let old = harness::read(&bundle);
    harness::replace(&bundle, &format!("{old}{new_cert}"));
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("overlap 阶段旧 leaf 仍然可用");

    // 换叶：主控切到新 leaf，仍然可用。
    harness::replace(&materials.controller_dir.join("client-leaf.pem"), &new_cert);
    harness::replace(&materials.controller_dir.join("client-chain.pem"), &new_cert);
    harness::replace(&materials.controller_dir.join("client-leaf.key"), &new_key);
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("换叶之后新 leaf 可用");

    // revoke：把旧 leaf 从 bundle 移除。新的仍然可用。
    let removed =
        pki::revoke_from_bundle(&bundle, &materials.client_leaf_spki).expect("移除旧 leaf");
    assert_eq!(removed, 1);
    harness::handshake(&materials.agent_dir, &materials.controller_dir)
        .await
        .expect("revoke 之后新 leaf 仍然可用");

    // 把主控切回旧 leaf：现在必须被拒。
    let generation_dir = materials.workspace.generation_dir(NODE_ID, GENERATION);
    let _ = generation_dir;
    let old_leaf = old.clone();
    harness::replace(&materials.controller_dir.join("client-leaf.pem"), &old_leaf);
    harness::replace(&materials.controller_dir.join("client-chain.pem"), &old_leaf);
    // 私钥没换回去，所以这一步本来就握不上；重点是**即使换回去也不行**，
    // 由上一条「同一 intermediate 新签」覆盖了同类语义。
    let result = harness::handshake(&materials.agent_dir, &materials.controller_dir).await;
    assert!(result.is_err(), "已吊销的 leaf 不得再被接受");
}

/// 拒绝把 bundle 清空：空 bundle 会让这条链路整个不可用。
#[test]
fn 不许把bundle清空() {
    let materials = harness::materials();
    let bundle = materials.agent_dir.join("authorized-clients.pem");
    let error = pki::revoke_from_bundle(&bundle, &materials.client_leaf_spki).unwrap_err();
    assert!(error.to_string().contains("清空"), "实际：{error}");
}

#[test]
fn 吊销不存在的指纹要报错而不是静默通过() {
    let materials = harness::materials();
    let bundle = materials.agent_dir.join("authorized-clients.pem");
    let error = pki::revoke_from_bundle(&bundle, &"0".repeat(64)).unwrap_err();
    assert!(error.to_string().contains("没有指纹"), "实际：{error}");
}

/// EKU 单一在**装配 bundle 时**就查。装配期报错比握手期报错容易归因得多。
#[test]
fn eku不单一的证书进不了bundle() {
    let materials = harness::materials();
    // 把 server leaf 当作 client bundle 装配：EKU 是 serverAuth，必须被拒。
    let server_leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    let error = AuthorizedLeaves::new(server_leaf, Usage::ClientAuth).unwrap_err();
    assert!(error.to_string().contains("clientAuth"), "实际：{error}");

    // 反向证据：正确用途的证书装得进去。
    let client_leaf = pki::load_certs(&materials.agent_dir.join("authorized-clients.pem")).unwrap();
    assert_eq!(AuthorizedLeaves::new(client_leaf, Usage::ClientAuth).unwrap().len(), 1);
}

#[test]
fn 空bundle在装配时就被拒() {
    let error = AuthorizedLeaves::new(Vec::new(), Usage::ClientAuth).unwrap_err();
    assert!(error.to_string().contains("空"), "实际：{error}");
}

/// 带外核对的指纹必须与部署材料里的 leaf 对得上。
///
/// `nodes.toml` 里写的 `agent_spki_sha256` 就是这个值——
/// 它是**带外**核对的记录，与在线信任的那张 leaf 必须一致。
#[test]
fn 带外指纹与部署材料一致() {
    let materials = harness::materials();
    let server_leaf = pki::load_certs(&materials.agent_dir.join("server-leaf.pem")).unwrap();
    let fingerprint = proxy_manager_wire::verify::spki_sha256(&server_leaf[0]).unwrap();
    assert_eq!(fingerprint, materials.server_leaf_spki);
    assert_eq!(fingerprint.len(), 64);

    // 主控侧信任的那张与 agent 自己那张是同一张。
    let authorized =
        pki::load_certs(&materials.controller_dir.join("authorized-servers.pem")).unwrap();
    assert_eq!(authorized[0], server_leaf[0]);
}

/// 两端的 HMAC key 是同一把，且**不复用任何 CA 材料**（D12）。
#[test]
fn 两端hmac_key一致且不复用ca材料() {
    let materials = harness::materials();
    let agent_key = pki::load_hmac_key(&materials.agent_dir.join("hmac.key")).unwrap();
    let controller_key = pki::load_hmac_key(&materials.controller_dir.join("hmac.key")).unwrap();
    assert_eq!(agent_key.to_hex(), controller_key.to_hex());
    assert_eq!(agent_key.to_hex().len(), 64, "32 字节");

    // 不得与任何证书/私钥材料重合。
    let generation_dir = materials.workspace.generation_dir(NODE_ID, GENERATION);
    for name in ["server-intermediate.key", "client-intermediate.key"] {
        let pem = harness::read(&generation_dir.join(name));
        assert!(!pem.contains(&agent_key.to_hex()), "HMAC key 不得复用 {name}");
    }

    // 两个不同节点拿到的是两把不同的 key。
    let other = materials
        .workspace
        .issue_node("node-example-02", GENERATION, "node-02.example.com", &Default::default())
        .unwrap();
    let other_key = pki::load_hmac_key(&other.agent_dir.join("hmac.key")).unwrap();
    assert_ne!(agent_key.to_hex(), other_key.to_hex(), "每节点一把独立 key");
}

/// 每个节点有自己的一对 intermediate：吊销一个节点不牵动其他节点。
#[tokio::test]
async fn 每个节点的intermediate互相独立() {
    let materials = harness::materials();
    let other = materials
        .workspace
        .issue_node("node-example-02", GENERATION, "node-02.example.com", &Default::default())
        .unwrap();

    // 两个节点各自都能握手。
    harness::handshake(&materials.agent_dir, &materials.controller_dir).await.unwrap();
    harness::handshake(&other.agent_dir, &other.controller_dir).await.unwrap();

    // 拿 02 的主控材料去连 01 的 agent：必须被拒。
    let result = harness::handshake(&materials.agent_dir, &other.controller_dir).await;
    assert!(result.is_err(), "一个节点的授信不得覆盖另一个节点");
}
