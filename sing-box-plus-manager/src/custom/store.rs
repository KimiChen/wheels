//! 自定义节点的读写与**命名冲突判定**。
//!
//! # 为什么冲突判定这么啰嗦
//!
//! 订阅是一份 Clash 配置，而 Clash 对重名的反应是**拒绝整份配置**——
//! 不是忽略其中一条。所以一个人建了两个同名节点，后果不是「少一个节点」，
//! 而是他的订阅**整个不能用**，客户端只说一句「订阅格式错误」。
//!
//! 而重名有四种来源，少判一种就漏一种：
//!
//! 1. 与受管入口或代理组重名（`reserved`）；
//! 2. 与自己已有的上游或 SOCKS5 重名；
//! 3. 与某条 SOCKS5 **自动生成的拨号组**重名；
//! 4. 新建 SOCKS5 时，它自己要生成的拨号组与上述任何一个重名。
//!
//! 第 3、4 条最容易漏：那个组名是渲染时凭空造出来的，库里没有这一行。
//!
//! # 渲染时还会再判一次
//!
//! 这里的判定全在应用层，而应用层会被绕过（直接写库、备份恢复、导入脚本）。
//! `subscription::render` 里那次重名与引用检查是兜底，判不过就让整份订阅
//! 失败而不是发一份坏的出去。

use crate::custom::crypto::{self, CryptoBox, Kind};
use crate::custom::parse::{CustomProxy, CustomSocks5, Protocol};
use crate::error::Error;
use crate::store::Store;

/// 服务层的错误。分三类是为了让 API 能映射到不同状态码：
/// 「你写错了」「和别的东西撞了」「它不存在」对调用方的处置完全不同。
#[derive(Debug)]
pub enum CustomError {
    Invalid(String),
    Conflict(String),
    NotFound,
    Internal(Error),
}

impl From<Error> for CustomError {
    fn from(error: Error) -> Self {
        // 解析与校验走的是 `Error::Codec`，它们是**用户输入错误**而不是内部故障。
        // 不分开的话，一条「端口必须在 1 到 65535 之间」会变成 500，
        // 而页面上只会显示「服务器错误」——用户永远不知道自己哪里填错了。
        match error {
            Error::Codec(message) => CustomError::Invalid(message),
            other => CustomError::Internal(other),
        }
    }
}

impl From<sqlx::Error> for CustomError {
    fn from(error: sqlx::Error) -> Self {
        CustomError::Internal(Error::Store(error))
    }
}

type Result<T> = std::result::Result<T, CustomError>;

// ============ 渲染口径的名字 ============
//
// 库里存的是**不带前缀的原名**，前缀只在渲染时加。改前缀不该要求改库，
// 而前缀存在的理由是让人在客户端的节点列表里一眼看出哪些是自己加的。

pub fn custom_proxy_name(name: &str) -> String {
    format!("Custom-{name}")
}

pub fn socks5_node_name(name: &str) -> String {
    format!("Socks5-{name}")
}

/// 每条 SOCKS5 自动生成的「先选前置节点」组。
///
/// **SOCKS5 节点的 `dialer-proxy` 指向这个组，而不是直接指向某条上游。**
/// 多这一层的理由是：用户可以在客户端里直接换前置，不用回来改配置、
/// 不用重新导入订阅。旧站就是这么做的，这一层是它最值钱的一个设计。
pub fn dialer_group_name(name: &str) -> String {
    format!("{}-先选前置节点", socks5_node_name(name))
}

// ============ 行 ============

#[derive(Debug, Clone)]
pub struct StoredProxy {
    pub id: i64,
    pub protocol: Protocol,
    pub config: CustomProxy,
    pub created_at: String,
    pub updated_at: String,
}

#[derive(Debug, Clone)]
pub struct StoredSocks5 {
    pub id: i64,
    pub config: CustomSocks5,
    pub created_at: String,
    pub updated_at: String,
}

/// 这个人**当前可选的拨号上游**：DIRECT + 受管入口 + 自己的自定义上游。
///
/// 注意这里用的是**原名**（不带 `Custom-` 前缀）——库里存的是原名，
/// 渲染时才映射到带前缀的名字。两处用不同口径会让 dialer 指向一个
/// 渲染后并不存在的名字，而那正是 Clash 拒整份配置的条件之一。
pub fn upstream_names(managed_entries: &[&str], proxies: &[StoredProxy]) -> Vec<String> {
    let mut names = vec!["DIRECT".to_string()];
    names.extend(managed_entries.iter().map(|s| s.to_string()));
    names.extend(proxies.iter().map(|p| p.config.name.clone()));
    names.dedup();
    names
}

// ============ 读 ============

pub async fn list_proxies(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
) -> Result<Vec<StoredProxy>> {
    let rows: Vec<(i64, String, String, String, String)> = sqlx::query_as(
        "SELECT custom_proxy_id, protocol, config_ciphertext, created_at, updated_at \
         FROM custom_proxies WHERE user_id = ? ORDER BY custom_proxy_id",
    )
    .bind(user_id)
    .fetch_all(store.readers())
    .await?;
    let purpose = crypto::purpose(Kind::Proxy, user_id);
    let mut out = Vec::with_capacity(rows.len());
    for (id, protocol, ciphertext, created_at, updated_at) in rows {
        let plain = crypto.decrypt(&ciphertext, &purpose).map_err(CustomError::Internal)?;
        let config: CustomProxy = serde_json::from_str(&plain).map_err(|error| {
            CustomError::Internal(Error::Codec(format!("配置解码失败：{error}")))
        })?;
        out.push(StoredProxy {
            id,
            protocol: Protocol::parse(&protocol)?,
            config,
            created_at,
            updated_at,
        });
    }
    Ok(out)
}

pub async fn list_socks5(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
) -> Result<Vec<StoredSocks5>> {
    let rows: Vec<(i64, String, String, String)> = sqlx::query_as(
        "SELECT custom_socks5_id, config_ciphertext, created_at, updated_at \
         FROM custom_socks5 WHERE user_id = ? ORDER BY custom_socks5_id",
    )
    .bind(user_id)
    .fetch_all(store.readers())
    .await?;
    let purpose = crypto::purpose(Kind::Socks5, user_id);
    let mut out = Vec::with_capacity(rows.len());
    for (id, ciphertext, created_at, updated_at) in rows {
        let plain = crypto.decrypt(&ciphertext, &purpose).map_err(CustomError::Internal)?;
        let config: CustomSocks5 = serde_json::from_str(&plain).map_err(|error| {
            CustomError::Internal(Error::Codec(format!("配置解码失败：{error}")))
        })?;
        out.push(StoredSocks5 { id, config, created_at, updated_at });
    }
    Ok(out)
}

// ============ 冲突判定 ============

/// 这个名字能不能用。`exclude` 是「改自己」时要跳过的那一条。
fn assert_name_available(
    name: &str,
    reserved: &[&str],
    proxies: &[StoredProxy],
    socks: &[StoredSocks5],
    exclude: Option<(Kind, i64)>,
) -> Result<()> {
    if reserved.contains(&name) {
        return Err(CustomError::Conflict(format!("「{name}」与受管入口或代理组重名")));
    }
    for proxy in proxies {
        if exclude == Some((Kind::Proxy, proxy.id)) {
            continue;
        }
        if proxy.config.name == name {
            return Err(CustomError::Conflict(format!("「{name}」已被自己的另一条上游使用")));
        }
    }
    for item in socks {
        if exclude == Some((Kind::Socks5, item.id)) {
            continue;
        }
        if item.config.name == name {
            return Err(CustomError::Conflict(format!("「{name}」已被自己的另一条 SOCKS5 使用")));
        }
        // 第 3 种来源：某条 SOCKS5 **自动生成的**拨号组名。库里没有这一行，
        // 不专门比就漏掉，而漏掉的后果是订阅整份不可用。
        if dialer_group_name(&item.config.name) == name {
            return Err(CustomError::Conflict(format!("「{name}」与已有的 SOCKS5 拨号组重名")));
        }
    }
    Ok(())
}

/// 新建/改名一条 SOCKS5 时，**它自己要生成的那个组名**也要判一遍。
fn assert_generated_group_available(
    node_name: &str,
    reserved: &[&str],
    proxies: &[StoredProxy],
    socks: &[StoredSocks5],
    exclude_id: Option<i64>,
) -> Result<()> {
    let generated = dialer_group_name(node_name);
    if reserved.contains(&generated.as_str()) {
        return Err(CustomError::Conflict(format!(
            "自动生成的拨号组「{generated}」与受管入口或代理组重名"
        )));
    }
    if proxies.iter().any(|p| p.config.name == generated) {
        return Err(CustomError::Conflict(format!(
            "自动生成的拨号组「{generated}」与自己的一条上游重名"
        )));
    }
    for item in socks {
        if exclude_id == Some(item.id) {
            continue;
        }
        if dialer_group_name(&item.config.name) == generated {
            return Err(CustomError::Conflict(format!("自动生成的拨号组「{generated}」重复")));
        }
    }
    Ok(())
}

fn assert_dialer_available(dialer: &str, managed: &[&str], proxies: &[StoredProxy]) -> Result<()> {
    if !upstream_names(managed, proxies).iter().any(|name| name == dialer) {
        return Err(CustomError::Invalid(format!(
            "前置节点「{dialer}」不可用：只能选 DIRECT、受管入口，或自己的一条自定义上游"
        )));
    }
    Ok(())
}

/// 一条上游被某条 SOCKS5 引用着就不能删、不能改名。
///
/// 允许的话，那条 SOCKS5 的 `dialer_proxy` 会指向一个不存在的名字，
/// 而渲染时那是「整份订阅失败」而不是「这一条不可用」。
fn assert_not_referenced(name: &str, socks: &[StoredSocks5]) -> Result<()> {
    if let Some(item) = socks.iter().find(|s| s.config.dialer_proxy == name) {
        return Err(CustomError::Conflict(format!(
            "「{name}」仍被 SOCKS5「{}」当作前置节点，先改掉它再操作",
            item.config.name
        )));
    }
    Ok(())
}

// ============ 写 ============

fn now() -> String {
    crate::ledger::bucket::to_rfc3339(time::OffsetDateTime::now_utc())
}

pub async fn create_proxy(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
    reserved: &[&str],
    config: CustomProxy,
) -> Result<i64> {
    let proxies = list_proxies(store, crypto, user_id).await?;
    let socks = list_socks5(store, crypto, user_id).await?;
    assert_name_available(&config.name, reserved, &proxies, &socks, None)?;

    let sealed = seal(crypto, Kind::Proxy, user_id, &config)?;
    let stamp = now();
    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    let id: i64 = sqlx::query_scalar(
        "INSERT INTO custom_proxies(user_id, name, protocol, config_version, \
         config_ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) \
         RETURNING custom_proxy_id",
    )
    .bind(user_id)
    .bind(&config.name)
    .bind(config.protocol.as_str())
    .bind(crypto::CURRENT_VERSION)
    .bind(&sealed)
    .bind(&stamp)
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(id)
}

pub async fn update_proxy(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
    reserved: &[&str],
    id: i64,
    config: CustomProxy,
) -> Result<()> {
    let proxies = list_proxies(store, crypto, user_id).await?;
    let socks = list_socks5(store, crypto, user_id).await?;
    let current = proxies.iter().find(|p| p.id == id).ok_or(CustomError::NotFound)?;
    if current.config.name != config.name {
        assert_not_referenced(&current.config.name, &socks)?;
    }
    assert_name_available(&config.name, reserved, &proxies, &socks, Some((Kind::Proxy, id)))?;

    let sealed = seal(crypto, Kind::Proxy, user_id, &config)?;
    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    sqlx::query(
        "UPDATE custom_proxies SET name = ?, protocol = ?, config_version = ?, \
         config_ciphertext = ?, updated_at = ? WHERE custom_proxy_id = ? AND user_id = ?",
    )
    .bind(&config.name)
    .bind(config.protocol.as_str())
    .bind(crypto::CURRENT_VERSION)
    .bind(&sealed)
    .bind(now())
    .bind(id)
    // **user_id 必须进 WHERE。** 少了它，改的 id 只要存在就能改到别人那一条。
    .bind(user_id)
    .execute(txn.conn())
    .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(())
}

pub async fn delete_proxy(store: &Store, crypto: &CryptoBox, user_id: i64, id: i64) -> Result<()> {
    let proxies = list_proxies(store, crypto, user_id).await?;
    let current = proxies.iter().find(|p| p.id == id).ok_or(CustomError::NotFound)?;
    let socks = list_socks5(store, crypto, user_id).await?;
    assert_not_referenced(&current.config.name, &socks)?;

    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    sqlx::query("DELETE FROM custom_proxies WHERE custom_proxy_id = ? AND user_id = ?")
        .bind(id)
        .bind(user_id)
        .execute(txn.conn())
        .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(())
}

pub async fn create_socks5(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
    reserved: &[&str],
    managed: &[&str],
    config: CustomSocks5,
) -> Result<i64> {
    let proxies = list_proxies(store, crypto, user_id).await?;
    let socks = list_socks5(store, crypto, user_id).await?;
    assert_name_available(&config.name, reserved, &proxies, &socks, None)?;
    assert_dialer_available(&config.dialer_proxy, managed, &proxies)?;
    assert_generated_group_available(&config.name, reserved, &proxies, &socks, None)?;

    let sealed = seal(crypto, Kind::Socks5, user_id, &config)?;
    let stamp = now();
    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    let id: i64 = sqlx::query_scalar(
        "INSERT INTO custom_socks5(user_id, name, config_version, config_ciphertext, \
         created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING custom_socks5_id",
    )
    .bind(user_id)
    .bind(&config.name)
    .bind(crypto::CURRENT_VERSION)
    .bind(&sealed)
    .bind(&stamp)
    .bind(&stamp)
    .fetch_one(txn.conn())
    .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(id)
}

pub async fn update_socks5(
    store: &Store,
    crypto: &CryptoBox,
    user_id: i64,
    reserved: &[&str],
    managed: &[&str],
    id: i64,
    config: CustomSocks5,
) -> Result<()> {
    let proxies = list_proxies(store, crypto, user_id).await?;
    let socks = list_socks5(store, crypto, user_id).await?;
    if !socks.iter().any(|s| s.id == id) {
        return Err(CustomError::NotFound);
    }
    assert_name_available(&config.name, reserved, &proxies, &socks, Some((Kind::Socks5, id)))?;
    assert_dialer_available(&config.dialer_proxy, managed, &proxies)?;
    assert_generated_group_available(&config.name, reserved, &proxies, &socks, Some(id))?;

    let sealed = seal(crypto, Kind::Socks5, user_id, &config)?;
    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    sqlx::query(
        "UPDATE custom_socks5 SET name = ?, config_version = ?, config_ciphertext = ?, \
         updated_at = ? WHERE custom_socks5_id = ? AND user_id = ?",
    )
    .bind(&config.name)
    .bind(crypto::CURRENT_VERSION)
    .bind(&sealed)
    .bind(now())
    .bind(id)
    .bind(user_id)
    .execute(txn.conn())
    .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(())
}

pub async fn delete_socks5(store: &Store, crypto: &CryptoBox, user_id: i64, id: i64) -> Result<()> {
    let socks = list_socks5(store, crypto, user_id).await?;
    if !socks.iter().any(|s| s.id == id) {
        return Err(CustomError::NotFound);
    }
    let mut txn = store.begin_immediate().await.map_err(CustomError::Internal)?;
    sqlx::query("DELETE FROM custom_socks5 WHERE custom_socks5_id = ? AND user_id = ?")
        .bind(id)
        .bind(user_id)
        .execute(txn.conn())
        .await?;
    txn.commit().await.map_err(CustomError::Internal)?;
    Ok(())
}

fn seal<T: serde::Serialize>(
    crypto: &CryptoBox,
    kind: Kind,
    user_id: i64,
    config: &T,
) -> Result<String> {
    let plain = serde_json::to_string(config)
        .map_err(|error| CustomError::Internal(Error::Codec(format!("配置编码失败：{error}"))))?;
    crypto.encrypt(&plain, &crypto::purpose(kind, user_id)).map_err(CustomError::Internal)
}
