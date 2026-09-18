//! 双 CA PKI：签发、精确 leaf 授信、吊销与到期巡检（`docs/threat-model.md` §2）。
//!
//! **建 PKI 的第一步是写门禁，第二步才是生成密钥。** 顺序反了就会重演同类系统那件事：
//! PKI 文档明确写着「该目录及其备份不得进入 Git」，实际却有 118 个文件被跟踪，
//! 包括离线 root CA 私钥与发布签名私钥，且没有任何忽略规则覆盖它们。
//! 本项目的门禁在 `sing-box-plus-manager/.gitignore` 与
//! `tests/m1/secret_gate.rs`——**用例会真的去 git 里查**，不是一句嘱咐。

pub mod inspect;
pub mod issue;

use std::path::{Path, PathBuf};

use crate::error::{Error, Result};
use proxy_manager_wire::verify::Usage;
use proxy_manager_wire::HmacKey;

/// 装载与 TLS 配置在 `proxy-manager-wire` 里——**主控与 agent 必须按同一套规则判定信任**。
/// 这里只 re-export，不重写第二份。
pub use proxy_manager_wire::tls::{client_config, load_certs, load_hmac_key, server_config};

/// 离线 workspace 的布局。
///
/// root 与全部 intermediate 私钥**留在这里**，节点只拿 `deployment/agent/` 那一层。
pub struct Workspace {
    root: PathBuf,
}

impl Workspace {
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Workspace { root: root.into() }
    }

    pub fn path(&self) -> &Path {
        &self.root
    }

    pub fn root_cert_pem(&self) -> PathBuf {
        self.root.join("root").join("root-ca.pem")
    }

    pub fn root_key_pem(&self) -> PathBuf {
        self.root.join("root").join("root-ca.key")
    }

    pub fn serials_file(&self) -> PathBuf {
        self.root.join("serials.txt")
    }

    pub fn generation_dir(&self, node_id: &str, generation: &str) -> PathBuf {
        self.root.join("nodes").join(node_id).join(generation)
    }

    pub fn agent_deployment(&self, node_id: &str, generation: &str) -> PathBuf {
        self.generation_dir(node_id, generation).join("deployment").join("agent")
    }

    pub fn controller_deployment(&self, node_id: &str, generation: &str) -> PathBuf {
        self.generation_dir(node_id, generation).join("deployment").join("controller")
    }

    /// 已用过的 serial。`serial 唯一`这条校验靠它。
    pub fn used_serials(&self) -> Result<std::collections::BTreeSet<u64>> {
        let path = self.serials_file();
        if !path.exists() {
            return Ok(Default::default());
        }
        let text = std::fs::read_to_string(&path)
            .map_err(|e| Error::Pki(format!("读取 {} 失败：{e}", path.display())))?;
        Ok(text.lines().filter_map(|line| line.trim().parse::<u64>().ok()).collect())
    }

    fn record_serials(&self, serials: &[u64]) -> Result<()> {
        use std::io::Write;
        let mut file = std::fs::OpenOptions::new()
            .append(true)
            .create(true)
            .open(self.serials_file())
            .map_err(|e| Error::Pki(format!("记录 serial 失败：{e}")))?;
        for serial in serials {
            writeln!(file, "{serial}").map_err(|e| Error::Pki(format!("记录 serial 失败：{e}")))?;
        }
        Ok(())
    }

    /// 建 workspace 并生成离线 root。已存在则拒绝——**不覆盖任何私钥**。
    pub fn init(&self, validity: &issue::Validity) -> Result<()> {
        if self.root_key_pem().exists() {
            return Err(Error::Pki(format!(
                "{} 已存在。不覆盖既有 root 私钥：覆盖等于让所有已签发的材料一起失效，\
                 而且旧私钥没有第二份",
                self.root_key_pem().display()
            )));
        }
        issue::create_private_dir(&self.root)?;
        issue::create_private_dir(&self.root.join("root"))?;
        let serial = next_serial(&self.used_serials()?);
        let root = issue::generate_root(validity, serial)?;
        issue::write_public(&self.root_cert_pem(), &root.cert_pem())?;
        issue::write_secret(&self.root_key_pem(), &root.key_pem())?;
        self.record_serials(&[serial])?;
        Ok(())
    }

    pub fn load_root(&self) -> Result<issue::Issued> {
        let cert_pem = std::fs::read_to_string(self.root_cert_pem())
            .map_err(|e| Error::Pki(format!("读取 root 公证书失败：{e}（先跑 pki init）")))?;
        let key_pem = std::fs::read_to_string(self.root_key_pem())
            .map_err(|e| Error::Pki(format!("读取 root 私钥失败：{e}")))?;
        let key = rcgen::KeyPair::from_pem(&key_pem)
            .map_err(|e| Error::Pki(format!("root 私钥解析失败：{e}")))?;
        let params = rcgen::CertificateParams::from_ca_cert_pem(&cert_pem)
            .map_err(|e| Error::Pki(format!("root 公证书解析失败：{e}")))?;
        let cert = params
            .self_signed(&key)
            .map_err(|e| Error::Pki(format!("重建 root 签发上下文失败：{e}")))?;
        Ok(issue::Issued { cert, key, serial: 0 })
    }

    /// 为一个节点签发一个 generation 的完整材料。
    ///
    /// generation 用日期 + 序号命名（如 `2026-09-18-a`），轮换时新旧并存一段时间——
    /// 这是 `overlap → 换叶 → revoke` 三阶段里 overlap 那一步的前提。
    pub fn issue_node(
        &self,
        node_id: &str,
        generation: &str,
        dns_name: &str,
        validity: &issue::Validity,
    ) -> Result<NodeGeneration> {
        let root = self.load_root()?;
        let mut used = self.used_serials()?;
        let mut fresh = || {
            let serial = next_serial(&used);
            used.insert(serial);
            serial
        };

        let server_ca_serial = fresh();
        let client_ca_serial = fresh();
        let server_leaf_serial = fresh();
        let client_leaf_serial = fresh();

        // 每个节点有自己的一对 intermediate：吊销一个节点不牵动其他节点。
        let server_ca = issue::issue_intermediate(
            &root,
            &format!("proxy-manager server intermediate {node_id} {generation}"),
            validity,
            server_ca_serial,
        )?;
        let client_ca = issue::issue_intermediate(
            &root,
            &format!("proxy-manager client intermediate {node_id} {generation}"),
            validity,
            client_ca_serial,
        )?;
        let server_leaf =
            issue::issue_server_leaf(&server_ca, node_id, dns_name, validity, server_leaf_serial)?;
        let client_leaf =
            issue::issue_client_leaf(&client_ca, node_id, validity, client_leaf_serial)?;

        // 签发阶段的六项校验。运行时不一定有能力再做一次。
        let before = self.used_serials()?;
        issue::verify_issued(&issue::IssuanceCheck {
            der: &server_leaf.der(),
            key_pem: &server_leaf.key_pem(),
            expected_usage: Usage::ServerAuth,
            expected_sans: &[
                format!("DNS:{dns_name}"),
                format!("URI:{}{node_id}", issue::NODE_URI_PREFIX),
            ],
            serial: server_leaf_serial,
            used_serials: &before,
        })?;
        issue::verify_issued(&issue::IssuanceCheck {
            der: &client_leaf.der(),
            key_pem: &client_leaf.key_pem(),
            expected_usage: Usage::ClientAuth,
            expected_sans: &[format!("URI:{}{node_id}", issue::CONTROLLER_URI_PREFIX)],
            serial: client_leaf_serial,
            used_serials: &before,
        })?;

        // 每节点一把独立 HMAC key，不复用任何 CA 材料（D12）。
        let hmac = HmacKey::generate();

        let generation_dir = self.generation_dir(node_id, generation);
        issue::create_private_dir(&generation_dir)?;
        issue::write_public(
            &generation_dir.join("server-intermediate.pem"),
            &server_ca.cert_pem(),
        )?;
        issue::write_secret(&generation_dir.join("server-intermediate.key"), &server_ca.key_pem())?;
        issue::write_public(
            &generation_dir.join("client-intermediate.pem"),
            &client_ca.cert_pem(),
        )?;
        issue::write_secret(&generation_dir.join("client-intermediate.key"), &client_ca.key_pem())?;

        let agent_dir = self.agent_deployment(node_id, generation);
        let controller_dir = self.controller_deployment(node_id, generation);
        issue::create_private_dir(&agent_dir)?;
        issue::create_private_dir(&controller_dir)?;

        // agent 侧：自己的 server leaf + 授权的 client leaf（信任锚）。
        issue::write_public(&agent_dir.join("server-leaf.pem"), &server_leaf.cert_pem())?;
        issue::write_secret(&agent_dir.join("server-leaf.key"), &server_leaf.key_pem())?;
        // chain 只用于让标准客户端的证书选择器能匹配到并发送完整链；
        // **对端并不信任其中的 intermediate**。
        issue::write_public(
            &agent_dir.join("server-chain.pem"),
            &format!("{}{}", server_leaf.cert_pem(), server_ca.cert_pem()),
        )?;
        issue::write_public(&agent_dir.join("authorized-clients.pem"), &client_leaf.cert_pem())?;
        issue::write_secret(&agent_dir.join("hmac.key"), &format!("{}\n", hmac.to_hex()))?;

        // 主控侧：自己的 client leaf + 授权的 server leaf（信任锚）。
        issue::write_public(&controller_dir.join("client-leaf.pem"), &client_leaf.cert_pem())?;
        issue::write_secret(&controller_dir.join("client-leaf.key"), &client_leaf.key_pem())?;
        issue::write_public(
            &controller_dir.join("client-chain.pem"),
            &format!("{}{}", client_leaf.cert_pem(), client_ca.cert_pem()),
        )?;
        issue::write_public(
            &controller_dir.join("authorized-servers.pem"),
            &server_leaf.cert_pem(),
        )?;
        issue::write_secret(&controller_dir.join("hmac.key"), &format!("{}\n", hmac.to_hex()))?;

        // 部署文件集最小 + 权限正确。
        issue::verify_deployment_set(&agent_dir, issue::AGENT_DEPLOYMENT_FILES)?;
        issue::verify_deployment_set(&controller_dir, issue::CONTROLLER_DEPLOYMENT_FILES)?;
        for dir in [&agent_dir, &controller_dir] {
            issue::verify_permissions(dir)?;
            for entry in
                std::fs::read_dir(dir).map_err(|e| Error::Pki(format!("遍历部署目录失败：{e}")))?
            {
                let entry = entry.map_err(|e| Error::Pki(format!("遍历部署目录失败：{e}")))?;
                issue::verify_permissions(&entry.path())?;
            }
        }

        self.record_serials(&[
            server_ca_serial,
            client_ca_serial,
            server_leaf_serial,
            client_leaf_serial,
        ])?;

        Ok(NodeGeneration {
            node_id: node_id.to_string(),
            generation: generation.to_string(),
            agent_dir,
            controller_dir,
            server_leaf_spki: proxy_manager_wire::verify::spki_sha256(&server_leaf.der())
                .map_err(|e| Error::Pki(e.to_string()))?,
            client_leaf_spki: proxy_manager_wire::verify::spki_sha256(&client_leaf.der())
                .map_err(|e| Error::Pki(e.to_string()))?,
        })
    }
}

/// 从 PEM 重建一个签发上下文（CA 或 intermediate）。
///
/// 轮换时要用**同一个 intermediate** 再签一张新 leaf，所以这条路径是真实需求，
/// 不只是测试用的。它同时也是负向矩阵的入口：由同一 intermediate 新签的 leaf
/// **必须**在握手时被拒——那正是「在线信任的是精确 leaf，不是 intermediate」的含义。
pub fn load_ca(cert_pem: &str, key_pem: &str) -> Result<issue::Issued> {
    let key =
        rcgen::KeyPair::from_pem(key_pem).map_err(|e| Error::Pki(format!("私钥解析失败：{e}")))?;
    let params = rcgen::CertificateParams::from_ca_cert_pem(cert_pem)
        .map_err(|e| Error::Pki(format!("公证书解析失败：{e}")))?;
    let cert =
        params.self_signed(&key).map_err(|e| Error::Pki(format!("重建签发上下文失败：{e}")))?;
    Ok(issue::Issued { cert, key, serial: 0 })
}

#[derive(Debug, Clone)]
pub struct NodeGeneration {
    pub node_id: String,
    pub generation: String,
    pub agent_dir: PathBuf,
    pub controller_dir: PathBuf,
    /// 带外核对用的指纹。写进 `nodes.toml` 的 `agent_spki_sha256`。
    pub server_leaf_spki: String,
    pub client_leaf_spki: String,
}

fn next_serial(used: &std::collections::BTreeSet<u64>) -> u64 {
    use rand::Rng;
    let mut rng = rand::thread_rng();
    loop {
        // 避开 0，并留在 63 位内，免得某些解析器把最高位当符号位。
        let candidate: u64 = rng.gen_range(1..(1u64 << 62));
        if !used.contains(&candidate) {
            return candidate;
        }
    }
}

/// 吊销：**从授权 bundle 里移除**指定指纹的 leaf。
///
/// 证书上的 `REVOKED` 标记只是离线审计记录，不会让任何在线组件拒绝谁
/// （`docs/threat-model.md` §2.3）。真正的在线吊销就是这个函数做的事。
///
/// 三阶段顺序不能反：`overlap → 换叶 → revoke`，reload **先节点后主控**——
/// 反过来会在切换窗口里把自己关在门外。
pub fn revoke_from_bundle(bundle: &Path, spki_sha256: &str) -> Result<usize> {
    let certs = load_certs(bundle)?;
    let mut kept = Vec::new();
    let mut removed = 0usize;
    for der in certs {
        let fingerprint =
            proxy_manager_wire::verify::spki_sha256(&der).map_err(|e| Error::Pki(e.to_string()))?;
        if fingerprint == spki_sha256 {
            removed += 1;
        } else {
            kept.push(der);
        }
    }
    if removed == 0 {
        return Err(Error::Pki(format!("bundle 里没有指纹 {spki_sha256} 的证书")));
    }
    if kept.is_empty() {
        return Err(Error::Pki(
            "拒绝把 bundle 清空：空 bundle 会让这条链路整个不可用。\
             先按 overlap 阶段放入新 leaf，再移除旧的"
                .into(),
        ));
    }
    let pem: String = kept.iter().map(|der| pem_encode(der)).collect();
    issue::write_public(&bundle.to_path_buf(), &pem)?;
    Ok(removed)
}

fn pem_encode(der: &[u8]) -> String {
    use std::fmt::Write;
    const WRAP: usize = 64;
    let encoded = base64_standard(der);
    let mut out = String::from("-----BEGIN CERTIFICATE-----\n");
    for chunk in encoded.as_bytes().chunks(WRAP) {
        let _ = writeln!(out, "{}", std::str::from_utf8(chunk).unwrap_or_default());
    }
    out.push_str("-----END CERTIFICATE-----\n");
    out
}

fn base64_standard(input: &[u8]) -> String {
    const ALPHABET: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let b = [chunk[0], *chunk.get(1).unwrap_or(&0), *chunk.get(2).unwrap_or(&0)];
        let n = ((b[0] as u32) << 16) | ((b[1] as u32) << 8) | b[2] as u32;
        out.push(ALPHABET[(n >> 18) as usize & 63] as char);
        out.push(ALPHABET[(n >> 12) as usize & 63] as char);
        out.push(if chunk.len() > 1 { ALPHABET[(n >> 6) as usize & 63] as char } else { '=' });
        out.push(if chunk.len() > 2 { ALPHABET[n as usize & 63] as char } else { '=' });
    }
    out
}
