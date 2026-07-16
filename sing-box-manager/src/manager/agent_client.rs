//! Manager→Agent 客户端抽象。生产用 [`RustlsAgentClient`]（mTLS reqwest，PinnedServerVerifier +
//! Manager 客户端身份）；调度器/轮询测试用 [`MockAgentClient`]。核心可测缝：一切 Agent 交互经此 trait。

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;

use crate::domain::agent::{CommandKind, StatusReport};
use crate::domain::metering::{MeterBatchResponse, StatsBatch};
use crate::error::{AppError, ErrorCode, Result};
use crate::pki::verify::PinnedServerVerifier;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};

/// Agent 调用失败分类。`Timeout`/`Connect` 视为不可达（→offline）；其余视为错误（→error）。
#[derive(Debug, Clone)]
pub enum AgentError {
    Timeout,
    Connect,
    Tls,
    Http(u16),
    Decode,
    Other(String),
}

impl std::fmt::Display for AgentError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            AgentError::Timeout => write!(f, "timeout"),
            AgentError::Connect => write!(f, "connect"),
            AgentError::Tls => write!(f, "tls"),
            AgentError::Http(c) => write!(f, "http_{c}"),
            AgentError::Decode => write!(f, "decode"),
            AgentError::Other(s) => write!(f, "other:{s}"),
        }
    }
}

/// Agent 命令响应（已由 Agent 脱敏）。
#[derive(Debug, Clone)]
pub struct AgentResponse {
    pub http_status: u16,
    pub ok: bool,
    pub body_json: String,
    pub echo_command_id: Option<String>,
}

#[async_trait]
pub trait AgentClient: Send + Sync {
    async fn get_status(
        &self,
        host_id: &str,
        mgmt_address: &str,
    ) -> std::result::Result<StatusReport, AgentError>;

    /// GET /v1/sing-box/stats（Phase 5 计量；纯只读，不经 agent_commands）。
    async fn get_stats(
        &self,
        host_id: &str,
        mgmt_address: &str,
    ) -> std::result::Result<StatsBatch, AgentError>;

    async fn post_command(
        &self,
        host_id: &str,
        mgmt_address: &str,
        kind: CommandKind,
        command_id: &str,
        body_json: &str,
    ) -> std::result::Result<AgentResponse, AgentError>;

    async fn get_deployment(
        &self,
        host_id: &str,
        mgmt_address: &str,
        command_id: &str,
    ) -> std::result::Result<Option<AgentResponse>, AgentError>;

    /// GET /v1/deployments/{id}/meter-batch（结算屏障：取旧进程最终统计批）。
    async fn get_meter_batch(
        &self,
        host_id: &str,
        mgmt_address: &str,
        command_id: &str,
    ) -> std::result::Result<MeterBatchResponse, AgentError>;
}

// ---------- 真实 mTLS 客户端 ----------

/// Agent 命令类型 → Agent HTTP 路径。
fn command_path(kind: CommandKind, command_id: &str) -> String {
    match kind {
        CommandKind::Status => "/v1/status".into(),
        CommandKind::Stats => "/v1/sing-box/stats".into(),
        CommandKind::Users => "/v1/sing-box/users".into(),
        CommandKind::Reconcile => "/v1/sing-box/reconcile".into(),
        CommandKind::Deploy => "/v1/deployments".into(),
        CommandKind::MeterAck => format!("/v1/deployments/{command_id}/meter-ack"),
        CommandKind::Rollback => "/v1/rollback".into(),
    }
}

/// 构建 Manager 侧 mTLS ClientConfig：只信 agent_ca、pin host_id、出示 Manager 客户端证书。
pub fn build_client_config(
    agent_ca_pem: &str,
    manager_cert_pem: &str,
    manager_key_pem: &str,
    expected_host_id: &str,
) -> Result<rustls::ClientConfig> {
    let agent_ca_der = pem_one_cert(agent_ca_pem)?;
    let verifier = PinnedServerVerifier::new(agent_ca_der, expected_host_id.to_string())?;
    let chain = pem_certs(manager_cert_pem)?;
    let key = pem_private_key(manager_key_pem)?;
    let cfg = rustls::ClientConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|e| tls_err("协议版本", e))?
    .dangerous()
    .with_custom_certificate_verifier(verifier)
    .with_client_auth_cert(chain, key)
    .map_err(|e| tls_err("客户端证书", e))?;
    Ok(cfg)
}

/// 生产客户端：按 host_id 缓存 reqwest 客户端（各自 pin 不同 host）。
pub struct RustlsAgentClient {
    agent_ca_pem: String,
    manager_cert_pem: String,
    manager_key_pem: String,
    request_timeout: Duration,
    clients: tokio::sync::Mutex<HashMap<String, reqwest::Client>>,
}

impl RustlsAgentClient {
    pub fn new(agent_ca_pem: String, manager_cert_pem: String, manager_key_pem: String) -> Self {
        Self {
            agent_ca_pem,
            manager_cert_pem,
            manager_key_pem,
            request_timeout: Duration::from_secs(30),
            clients: tokio::sync::Mutex::new(HashMap::new()),
        }
    }

    async fn client_for(&self, host_id: &str) -> Result<reqwest::Client> {
        let mut guard = self.clients.lock().await;
        if let Some(c) = guard.get(host_id) {
            return Ok(c.clone());
        }
        let cfg = build_client_config(
            &self.agent_ca_pem,
            &self.manager_cert_pem,
            &self.manager_key_pem,
            host_id,
        )?;
        let client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .timeout(self.request_timeout)
            .connect_timeout(Duration::from_secs(5))
            .build()
            .map_err(|e| AppError::new(ErrorCode::Agent, format!("构建 HTTP 客户端失败: {e}")))?;
        guard.insert(host_id.to_string(), client.clone());
        Ok(client)
    }
}

fn map_reqwest_err(e: reqwest::Error) -> AgentError {
    if e.is_timeout() {
        AgentError::Timeout
    } else if e.is_connect() {
        AgentError::Connect
    } else if e.is_decode() {
        AgentError::Decode
    } else {
        AgentError::Other(e.to_string())
    }
}

#[async_trait]
impl AgentClient for RustlsAgentClient {
    async fn get_status(
        &self,
        host_id: &str,
        mgmt_address: &str,
    ) -> std::result::Result<StatusReport, AgentError> {
        let client = self
            .client_for(host_id)
            .await
            .map_err(|_| AgentError::Tls)?;
        let url = format!("https://{mgmt_address}/v1/status");
        let resp = client.get(&url).send().await.map_err(map_reqwest_err)?;
        if !resp.status().is_success() {
            return Err(AgentError::Http(resp.status().as_u16()));
        }
        resp.json::<StatusReport>()
            .await
            .map_err(|_| AgentError::Decode)
    }

    async fn get_stats(
        &self,
        host_id: &str,
        mgmt_address: &str,
    ) -> std::result::Result<StatsBatch, AgentError> {
        let client = self
            .client_for(host_id)
            .await
            .map_err(|_| AgentError::Tls)?;
        let url = format!("https://{mgmt_address}/v1/sing-box/stats");
        let resp = client.get(&url).send().await.map_err(map_reqwest_err)?;
        if !resp.status().is_success() {
            return Err(AgentError::Http(resp.status().as_u16()));
        }
        resp.json::<StatsBatch>()
            .await
            .map_err(|_| AgentError::Decode)
    }

    async fn post_command(
        &self,
        host_id: &str,
        mgmt_address: &str,
        kind: CommandKind,
        command_id: &str,
        body_json: &str,
    ) -> std::result::Result<AgentResponse, AgentError> {
        let client = self
            .client_for(host_id)
            .await
            .map_err(|_| AgentError::Tls)?;
        let url = format!("https://{mgmt_address}{}", command_path(kind, command_id));
        let resp = client
            .post(&url)
            .header("x-sbm-command-id", command_id)
            .header("content-type", "application/json")
            .body(body_json.to_string())
            .send()
            .await
            .map_err(map_reqwest_err)?;
        let http_status = resp.status().as_u16();
        let ok = resp.status().is_success();
        let echo = resp
            .headers()
            .get("x-sbm-command-id")
            .and_then(|v| v.to_str().ok())
            .map(|s| s.to_string());
        let body_json = resp.text().await.map_err(map_reqwest_err)?;
        Ok(AgentResponse {
            http_status,
            ok,
            body_json,
            echo_command_id: echo,
        })
    }

    async fn get_deployment(
        &self,
        host_id: &str,
        mgmt_address: &str,
        command_id: &str,
    ) -> std::result::Result<Option<AgentResponse>, AgentError> {
        let client = self
            .client_for(host_id)
            .await
            .map_err(|_| AgentError::Tls)?;
        let url = format!("https://{mgmt_address}/v1/deployments/{command_id}");
        let resp = client.get(&url).send().await.map_err(map_reqwest_err)?;
        if resp.status().as_u16() == 404 {
            return Ok(None);
        }
        let http_status = resp.status().as_u16();
        let ok = resp.status().is_success();
        let body_json = resp.text().await.map_err(map_reqwest_err)?;
        Ok(Some(AgentResponse {
            http_status,
            ok,
            body_json,
            echo_command_id: Some(command_id.to_string()),
        }))
    }

    async fn get_meter_batch(
        &self,
        host_id: &str,
        mgmt_address: &str,
        command_id: &str,
    ) -> std::result::Result<MeterBatchResponse, AgentError> {
        let client = self
            .client_for(host_id)
            .await
            .map_err(|_| AgentError::Tls)?;
        let url = format!("https://{mgmt_address}/v1/deployments/{command_id}/meter-batch");
        let resp = client.get(&url).send().await.map_err(map_reqwest_err)?;
        if resp.status().as_u16() == 404 {
            return Ok(MeterBatchResponse {
                batch: None,
                drain_clean: false,
            });
        }
        if !resp.status().is_success() {
            return Err(AgentError::Http(resp.status().as_u16()));
        }
        resp.json::<MeterBatchResponse>()
            .await
            .map_err(|_| AgentError::Decode)
    }
}

fn pem_certs(pem: &str) -> Result<Vec<CertificateDer<'static>>> {
    rustls_pemfile::certs(&mut pem.as_bytes())
        .collect::<std::result::Result<Vec<_>, _>>()
        .map_err(|e| AppError::new(ErrorCode::Crypto, format!("解析证书链失败: {e}")))
}

fn pem_one_cert(pem: &str) -> Result<CertificateDer<'static>> {
    pem_certs(pem)?
        .into_iter()
        .next()
        .ok_or_else(|| AppError::new(ErrorCode::Crypto, "PEM 无证书"))
}

fn pem_private_key(pem: &str) -> Result<PrivateKeyDer<'static>> {
    rustls_pemfile::private_key(&mut pem.as_bytes())
        .map_err(|e| AppError::new(ErrorCode::Crypto, format!("解析私钥失败: {e}")))?
        .ok_or_else(|| AppError::new(ErrorCode::Crypto, "PEM 无私钥"))
}

fn tls_err<E: std::fmt::Display>(what: &str, e: E) -> AppError {
    AppError::new(ErrorCode::Crypto, format!("{what}失败: {e}"))
}

// ---------- 测试用 Mock ----------

/// 脚本化 Mock：各方法从队列弹出预置结果。
pub struct MockAgentClient {
    pub status:
        std::sync::Mutex<std::collections::VecDeque<std::result::Result<StatusReport, AgentError>>>,
    pub post: std::sync::Mutex<
        std::collections::VecDeque<std::result::Result<AgentResponse, AgentError>>,
    >,
    pub deployment: std::sync::Mutex<
        std::collections::VecDeque<std::result::Result<Option<AgentResponse>, AgentError>>,
    >,
    pub stats:
        std::sync::Mutex<std::collections::VecDeque<std::result::Result<StatsBatch, AgentError>>>,
    pub meter_batch: std::sync::Mutex<
        std::collections::VecDeque<std::result::Result<MeterBatchResponse, AgentError>>,
    >,
}

impl Default for MockAgentClient {
    fn default() -> Self {
        Self {
            status: std::sync::Mutex::new(std::collections::VecDeque::new()),
            post: std::sync::Mutex::new(std::collections::VecDeque::new()),
            deployment: std::sync::Mutex::new(std::collections::VecDeque::new()),
            stats: std::sync::Mutex::new(std::collections::VecDeque::new()),
            meter_batch: std::sync::Mutex::new(std::collections::VecDeque::new()),
        }
    }
}

impl MockAgentClient {
    pub fn push_status(&self, r: std::result::Result<StatusReport, AgentError>) {
        self.status.lock().unwrap().push_back(r);
    }
    pub fn push_post(&self, r: std::result::Result<AgentResponse, AgentError>) {
        self.post.lock().unwrap().push_back(r);
    }
    pub fn push_deployment(&self, r: std::result::Result<Option<AgentResponse>, AgentError>) {
        self.deployment.lock().unwrap().push_back(r);
    }
    pub fn push_stats(&self, r: std::result::Result<StatsBatch, AgentError>) {
        self.stats.lock().unwrap().push_back(r);
    }
    pub fn push_meter_batch(&self, r: std::result::Result<MeterBatchResponse, AgentError>) {
        self.meter_batch.lock().unwrap().push_back(r);
    }
}

#[async_trait]
impl AgentClient for MockAgentClient {
    async fn get_status(
        &self,
        _host_id: &str,
        _mgmt_address: &str,
    ) -> std::result::Result<StatusReport, AgentError> {
        self.status
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Err(AgentError::Other("mock 无脚本".into())))
    }
    async fn get_stats(
        &self,
        _host_id: &str,
        _mgmt_address: &str,
    ) -> std::result::Result<StatsBatch, AgentError> {
        self.stats
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Err(AgentError::Other("mock 无 stats 脚本".into())))
    }
    async fn post_command(
        &self,
        _host_id: &str,
        _mgmt_address: &str,
        _kind: CommandKind,
        _command_id: &str,
        _body_json: &str,
    ) -> std::result::Result<AgentResponse, AgentError> {
        self.post
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Err(AgentError::Other("mock 无脚本".into())))
    }
    async fn get_deployment(
        &self,
        _host_id: &str,
        _mgmt_address: &str,
        _command_id: &str,
    ) -> std::result::Result<Option<AgentResponse>, AgentError> {
        self.deployment
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Ok(None))
    }
    async fn get_meter_batch(
        &self,
        _host_id: &str,
        _mgmt_address: &str,
        _command_id: &str,
    ) -> std::result::Result<MeterBatchResponse, AgentError> {
        self.meter_batch
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Ok(MeterBatchResponse {
                batch: None,
                drain_clean: false,
            }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pki::{generate_ca, install_ring_default, issue_manager_client_cert, Ca, CaRole};

    #[test]
    fn build_client_config_and_reqwest_client_from_generated_material() {
        // 脱网：只验证 mTLS ClientConfig 与 reqwest 客户端可从生成的证书材料构建（不发请求）。
        install_ring_default();
        let agent_ca = generate_ca(CaRole::AgentCa, 3650).unwrap();
        let client_ca = generate_ca(CaRole::ClientCa, 3650).unwrap();
        let cca = Ca::from_pem(&client_ca.cert_pem, &client_ca.key_pem).unwrap();
        let mc = issue_manager_client_cert(&cca, 2, 825).unwrap();
        let cfg =
            build_client_config(&agent_ca.cert_pem, &mc.cert_pem, &mc.key_pem, "host-1").unwrap();
        let _client = reqwest::Client::builder()
            .use_preconfigured_tls(cfg)
            .build()
            .unwrap();
    }
}
