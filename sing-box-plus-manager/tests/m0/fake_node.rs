//! 假节点：按 `sing-box-plus` 计量样例新建的 UDS 服务（M0 交付物）。
//!
//! 为什么必须新建，不能拿现成的凑：`shadowsocks-rust-plus/tests/mock_collector.py`
//! 是连接 auditd 的**审计客户端**，不会提供 `/v3/snapshot` 服务；
//! `golden_vectors.json` 覆盖的是 `/v1/audit/lease` 的 HMAC 与审计事件，
//! 没有计量快照的形状。文件名含 golden 不代表覆盖了目标协议
//! （`docs/integration-contract.md` §4）。
//!
//! 这份夹具镜像节点侧三处源码的行为，**不镜像任何一份 README**：
//! - 传输层 `internal/userstats/httpuds.go`：响应头形状、状态码文本、禁 query、
//!   禁分块、快照 socket 不收请求体。
//! - 快照路由 `internal/userstats/service.go`：`/v3/snapshot` 与 `/healthz` 两条路由，
//!   历史路径 `/v1`、`/v2` 恒 404，非 GET 返回 405。
//! - 配额端点 `internal/userstats/quota_endpoint.go` + `quota.go`：全量覆盖语义、
//!   409 的两条分支**共用同一个错误体**、未知身份整份 400 并回前若干条。
//!
//! 夹具向测试暴露 [`FakeNode::effective_quota`]：节点上**当前生效**的逐身份额度。
//! 这是 M0 完成标准里「通过生效表验证」那一句的落点——
//! 「PUT 返回了 200」不能证明零表收敛，因为一张空表同样会返回 200，
//! 而它的效果是把所有人解除限额。

#![allow(dead_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Mutex;

use proxy_manager::quota::Quota;

use super::fixtures;

/// 一次性的传输层故障注入。每个入队的故障只作用于下一个请求。
#[derive(Debug, Clone, PartialEq)]
pub enum TransportFault {
    /// 直接以该状态码回绝，**不进入处理逻辑**。
    /// 节点侧的 429 就是这种：并发上限在 accept 之后、handler 之前立即拒绝。
    Status(u16),
    /// 响应丢失：处理照常发生，但一个字节都不写回就关闭连接。
    /// 这是「节点已应用但 200 丢失」的精确形态——**结果不明，不是失败**。
    CloseWithoutResponse,
    /// 延迟到达：处理照常发生，等待之后才写回。
    /// 配合比它短的客户端超时，就能造出「主控超时了但节点已经应用」。
    Delay(Duration),
    /// 声明的 `Content-Length` 与实际写出的长度不符。
    ContentLengthDelta(i64),
    /// body 被截断，但仍声明完整长度。截断的 JSON 有可能仍然解析成功。
    TruncateBody(usize),
}

impl TransportFault {
    fn short_circuits(&self) -> bool {
        matches!(self, TransportFault::Status(_))
    }
}

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct QuotaCounters {
    pub rejected_by_runtime: u64,
    pub rejected_by_epoch: u64,
    pub rejected_by_unknown: u64,
}

struct State {
    snapshot: serde_json::Value,
    next_sequence: u64,
    faults: VecDeque<TransportFault>,

    quota_epoch: u64,
    quota_accepted: bool,
    /// 最后一次成功应用的全量表。**不在表里的身份 = 无限额度**（C16），
    /// 所以这里存的是「限额清单」而不是「用户清单」——
    /// [`FakeNode::effective_quota`] 按当前配置里的身份集合把它展开成生效视图。
    limited_table: BTreeMap<(String, String), i64>,
    applied_entries: usize,
    counters: QuotaCounters,

    snapshot_requests: u64,
    quota_requests: u64,
}

impl State {
    fn node_id(&self) -> String {
        self.snapshot["node_id"].as_str().unwrap_or_default().to_string()
    }

    fn runtime_id(&self) -> String {
        self.snapshot["runtime_id"].as_str().unwrap_or_default().to_string()
    }

    /// 当前配置中存在的计费身份。全量表要与**同一份配置真相**对齐，
    /// 对不上说明控制面与数据面已经不一致（`quota.go` 的 `ApplyQuotaTable` 注释）。
    fn identities(&self) -> BTreeSet<(String, String)> {
        let mut set = BTreeSet::new();
        let Some(inbounds) = self.snapshot["inbounds"].as_array() else {
            return set;
        };
        for inbound in inbounds {
            let tag = inbound["tag"].as_str().unwrap_or_default().to_string();
            let Some(users) = inbound["users"].as_array() else { continue };
            for user in users {
                set.insert((tag.clone(), user["name"].as_str().unwrap_or_default().to_string()));
            }
        }
        set
    }
}

pub struct FakeNode {
    _dir: tempfile::TempDir,
    pub snapshot_socket: PathBuf,
    pub quota_socket: PathBuf,
    state: Arc<Mutex<State>>,
    tasks: Vec<tokio::task::JoinHandle<()>>,
}

impl Drop for FakeNode {
    fn drop(&mut self) {
        for task in &self.tasks {
            task.abort();
        }
    }
}

impl FakeNode {
    /// 用上游导出的基线样例起一个假节点，两个 socket 分开监听（C18）。
    pub async fn start() -> Self {
        Self::start_with(fixtures::base_snapshot()).await
    }

    pub async fn start_with(snapshot: serde_json::Value) -> Self {
        let dir = tempfile::tempdir().expect("创建临时目录");
        let snapshot_socket = dir.path().join("user_stats.sock");
        let quota_socket = dir.path().join("user_stats_quota.sock");

        let next_sequence = snapshot["sequence"].as_u64().unwrap_or(1);
        let state = Arc::new(Mutex::new(State {
            snapshot,
            next_sequence,
            faults: VecDeque::new(),
            quota_epoch: 0,
            quota_accepted: false,
            limited_table: BTreeMap::new(),
            applied_entries: 0,
            counters: QuotaCounters::default(),
            snapshot_requests: 0,
            quota_requests: 0,
        }));

        let snapshot_listener = UnixListener::bind(&snapshot_socket).expect("绑定快照 socket");
        let quota_listener = UnixListener::bind(&quota_socket).expect("绑定配额 socket");

        let tasks = vec![
            tokio::spawn(serve(snapshot_listener, state.clone(), Route::Snapshot)),
            tokio::spawn(serve(quota_listener, state.clone(), Route::Quota)),
        ];

        FakeNode { _dir: dir, snapshot_socket, quota_socket, state, tasks }
    }

    // ---- 故障注入 ----

    pub async fn push_fault(&self, fault: TransportFault) {
        self.state.lock().await.faults.push_back(fault);
    }

    // ---- 快照体的状态变更 ----

    pub async fn set_schema_version(&self, version: u32) {
        let mut state = self.state.lock().await;
        state.snapshot["schema_version"] = serde_json::json!(version);
    }

    pub async fn set_health_bit(&self, key: &str, value: bool) {
        let mut state = self.state.lock().await;
        state.snapshot["health"][key] = serde_json::json!(value);
    }

    /// 给 health 加一个闭集之外的键（C4 的「多一个」）。
    pub async fn add_health_key(&self, key: &str, value: bool) {
        let mut state = self.state.lock().await;
        state.snapshot["health"]
            .as_object_mut()
            .expect("health 是对象")
            .insert(key.to_string(), serde_json::json!(value));
    }

    /// 删掉 health 的一个键（C4 的「少一个」）。
    pub async fn remove_health_key(&self, key: &str) {
        let mut state = self.state.lock().await;
        state.snapshot["health"].as_object_mut().expect("health 是对象").remove(key);
    }

    /// 设置**下一次** `/v3/snapshot` 返回的 sequence。
    /// 用来造重复、回退与跳号：三者在主控侧是三种不同的处置（C5、C26）。
    pub async fn set_next_sequence(&self, sequence: u64) {
        self.state.lock().await.next_sequence = sequence;
    }

    pub async fn set_counter(&self, inbound_tag: &str, user_name: &str, field: &str, value: u64) {
        let mut state = self.state.lock().await;
        let inbounds = state.snapshot["inbounds"].as_array_mut().expect("inbounds 是数组");
        for inbound in inbounds.iter_mut() {
            if inbound["tag"].as_str() != Some(inbound_tag) {
                continue;
            }
            let users = inbound["users"].as_array_mut().expect("users 是数组");
            for user in users.iter_mut() {
                if user["name"].as_str() == Some(user_name) {
                    user[field] = serde_json::json!(value);
                    return;
                }
            }
        }
        panic!("夹具里没有身份 {inbound_tag}/{user_name}");
    }

    /// 把一个已有身份**原样挂到另一条 inbound 上**：同名、四向计数归零。
    ///
    /// 这是「一个人同时持有 SS 与 VLESS」在节点侧的样子。名字在**每条 inbound 内**
    /// 唯一，跨 inbound 同名是合法的，节点侧的 `quotaCell` 也是按
    /// `(inbound_tag, name)` 挂的两个互不相干的计数器。
    ///
    /// 计数归零有两个理由：一条刚加出来的入口本来就没跑过流量；而且
    /// `pick_free_slot` 要求候选身份四向计数全为零，带着计数会让认领用例
    /// 失败在与它要测的东西无关的地方。
    pub async fn mirror_identity(
        &self,
        src_tag: &str,
        dst_tag: &str,
        dst_type: &str,
        user_name: &str,
    ) {
        let mut state = self.state.lock().await;
        let inbounds = state.snapshot["inbounds"].as_array_mut().expect("inbounds 是数组");
        let mut copied = inbounds
            .iter()
            .find(|inbound| inbound["tag"].as_str() == Some(src_tag))
            .and_then(|inbound| inbound["users"].as_array())
            .and_then(|users| users.iter().find(|user| user["name"].as_str() == Some(user_name)))
            .unwrap_or_else(|| panic!("夹具里没有身份 {src_tag}/{user_name}"))
            .clone();
        for field in
            ["tcp_uplink_bytes", "tcp_downlink_bytes", "udp_uplink_bytes", "udp_downlink_bytes"]
        {
            copied[field] = serde_json::json!(0u64);
        }

        if let Some(target) =
            inbounds.iter_mut().find(|inbound| inbound["tag"].as_str() == Some(dst_tag))
        {
            let users = target["users"].as_array_mut().expect("users 是数组");
            assert!(
                users.iter().all(|user| user["name"].as_str() != Some(user_name)),
                "同一条 inbound 内重名会被节点侧的校验直接拒掉：{dst_tag}/{user_name}"
            );
            // **按序插入，不能 push。** 解析器要求 `users[]` 按 (name, generation)
            // 的 ASCII 字节严格升序，乱序是「整份解析失败」——而结算失败在这一层
            // 是静默的，夹具只会表现成「主控没看见这个身份」。
            let key = |user: &serde_json::Value| {
                (
                    user["name"].as_str().unwrap_or_default().to_string(),
                    user["generation"].as_u64().unwrap_or_default(),
                )
            };
            let at = users.partition_point(|user| key(user) < key(&copied));
            users.insert(at, copied);
            return;
        }

        let listen_port = inbounds
            .iter()
            .filter_map(|inbound| inbound["listen_port"].as_u64())
            .max()
            .unwrap_or(8000)
            + 1;
        inbounds.push(serde_json::json!({
            "tag": dst_tag,
            "type": dst_type,
            "listen": "0.0.0.0",
            "listen_port": listen_port,
            "generation": 1,
            "active": true,
            "tcp_sessions": 0,
            "udp_sessions": 0,
            "users": [copied],
        }));
    }

    /// 让一个已观察过的 lineage 从后续快照里消失（C25）。
    pub async fn remove_identity(&self, inbound_tag: &str, user_name: &str) {
        let mut state = self.state.lock().await;
        let inbounds = state.snapshot["inbounds"].as_array_mut().expect("inbounds 是数组");
        for inbound in inbounds.iter_mut() {
            if inbound["tag"].as_str() != Some(inbound_tag) {
                continue;
            }
            let users = inbound["users"].as_array_mut().expect("users 是数组");
            users.retain(|user| user["name"].as_str() != Some(user_name));
        }
    }

    /// 模拟进程重启：换 `runtime_id`，累计值归零，sequence 重新从 1 开始，
    /// **额度状态全部丢失**（C12：额度是纯内存的）。
    ///
    /// 这正是节点侧 409 注释里写的「重启即解封」的自动纠正机制：
    /// 重启后 collector 拿旧 `runtime_id` 推送会立刻拿到 409。
    pub async fn restart(&self, new_runtime_id: &str) {
        let mut state = self.state.lock().await;
        state.snapshot["runtime_id"] = serde_json::json!(new_runtime_id);
        state.next_sequence = 1;
        state.quota_epoch = 0;
        state.quota_accepted = false;
        state.limited_table.clear();
        state.applied_entries = 0;
        if let Some(inbounds) = state.snapshot["inbounds"].as_array_mut() {
            for inbound in inbounds.iter_mut() {
                let Some(users) = inbound["users"].as_array_mut() else { continue };
                for user in users.iter_mut() {
                    for field in [
                        "tcp_uplink_bytes",
                        "tcp_downlink_bytes",
                        "udp_uplink_bytes",
                        "udp_downlink_bytes",
                    ] {
                        user[field] = serde_json::json!(0u64);
                    }
                }
            }
        }
    }

    /// 让节点改口说自己是另一个 `node_id`。
    ///
    /// 现实里的成因是「端点指错了」：两台机器的 agent 地址写反、或一条 DNS 记录
    /// 指向了别人。这正是 §4.5 409 判别第 1 步要挡的那件事。
    pub async fn set_node_id(&self, node_id: &str) {
        let mut state = self.state.lock().await;
        state.snapshot["node_id"] = serde_json::json!(node_id);
    }

    // ---- 向测试暴露的观察点 ----

    pub async fn node_id(&self) -> String {
        self.state.lock().await.node_id()
    }

    pub async fn runtime_id(&self) -> String {
        self.state.lock().await.runtime_id()
    }

    pub async fn identities(&self) -> BTreeSet<(String, String)> {
        self.state.lock().await.identities()
    }

    /// **当前生效的配额表**：节点上每个计费身份现在处于什么额度状态。
    ///
    /// 不在最后一份全量表里的身份是 `Unlimited`（C16）。因此「表里少了一个人」
    /// 与「那个人本来就无限额度」在这里是同一个观察结果——
    /// 反向用例故意省略某个用户后，必须能在这里看到他被解除限额。
    pub async fn effective_quota(&self) -> BTreeMap<(String, String), Quota> {
        let state = self.state.lock().await;
        state
            .identities()
            .into_iter()
            .map(|key| {
                let quota = match state.limited_table.get(&key) {
                    Some(value) => Quota::Limited(*value as u64),
                    None => Quota::Unlimited,
                };
                (key, quota)
            })
            .collect()
    }

    pub async fn quota_epoch(&self) -> u64 {
        self.state.lock().await.quota_epoch
    }

    pub async fn quota_accepted(&self) -> bool {
        self.state.lock().await.quota_accepted
    }

    pub async fn applied_entries(&self) -> usize {
        self.state.lock().await.applied_entries
    }

    pub async fn counters(&self) -> QuotaCounters {
        self.state.lock().await.counters
    }

    pub async fn snapshot_requests(&self) -> u64 {
        self.state.lock().await.snapshot_requests
    }

    pub async fn quota_requests(&self) -> u64 {
        self.state.lock().await.quota_requests
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Route {
    Snapshot,
    Quota,
}

impl Route {
    /// 快照 socket 不收请求体（`service.go` 起 exporter 时 `allowBody: false`）。
    fn allows_body(self) -> bool {
        matches!(self, Route::Quota)
    }
}

async fn serve(listener: UnixListener, state: Arc<Mutex<State>>, route: Route) {
    loop {
        let Ok((stream, _)) = listener.accept().await else { return };
        let state = state.clone();
        tokio::spawn(async move {
            handle_connection(stream, state, route).await;
        });
    }
}

async fn handle_connection(mut stream: UnixStream, state: Arc<Mutex<State>>, route: Route) {
    let Some(request) = read_request(&mut stream, route).await else {
        // 请求本身不合法：按节点侧的做法回 400 再关。
        write_response(&mut stream, 400, error_body(400), None, None).await;
        return;
    };

    let fault = state.lock().await.faults.pop_front();

    let (status, body) = match &fault {
        Some(fault) if fault.short_circuits() => match fault {
            TransportFault::Status(code) => (*code, error_body(*code)),
            _ => unreachable!(),
        },
        _ => match route {
            Route::Snapshot => handle_snapshot(&request, &state).await,
            Route::Quota => handle_quota(&request, &state).await,
        },
    };

    let mut length_delta = None;
    let mut truncate = None;
    match fault {
        Some(TransportFault::CloseWithoutResponse) => {
            // 一个字节都不写。客户端会看到「响应缺少头体分隔」，
            // 而节点侧的状态**已经变了**——这正是「结果不明」的定义。
            return;
        }
        Some(TransportFault::Delay(duration)) => tokio::time::sleep(duration).await,
        Some(TransportFault::ContentLengthDelta(delta)) => length_delta = Some(delta),
        Some(TransportFault::TruncateBody(keep)) => truncate = Some(keep),
        _ => {}
    }

    write_response(&mut stream, status, body, length_delta, truncate).await;
}

struct Request {
    method: String,
    target: String,
    body: Vec<u8>,
}

async fn read_request(stream: &mut UnixStream, route: Route) -> Option<Request> {
    let mut raw = Vec::new();
    let mut chunk = [0u8; 4096];
    let head_end = loop {
        if let Some(index) = find(&raw, b"\r\n\r\n") {
            break index;
        }
        match stream.read(&mut chunk).await {
            Ok(0) => return None,
            Ok(n) => raw.extend_from_slice(&chunk[..n]),
            Err(_) => return None,
        }
        if raw.len() > 64 * 1024 {
            return None;
        }
    };

    let head = std::str::from_utf8(&raw[..head_end]).ok()?;
    let mut lines = head.split("\r\n");
    let request_line = lines.next()?;
    let parts: Vec<&str> = request_line.split(' ').collect();
    if parts.len() != 3 || parts[2] != "HTTP/1.1" {
        return None;
    }
    let (method, target) = (parts[0].to_string(), parts[1].to_string());
    // 禁 query：节点侧对带 query 的请求一律 400，不做「忽略未知参数」的宽容处理。
    if target.contains('?') || target.contains('#') || !target.starts_with('/') {
        return None;
    }

    let mut content_length = 0usize;
    for line in lines {
        if line.is_empty() {
            continue;
        }
        let (name, value) = line.split_once(':')?;
        let name = name.trim().to_ascii_lowercase();
        if name == "transfer-encoding" {
            return None; // 不支持分块
        }
        if name == "content-length" {
            content_length = value.trim().parse().ok()?;
        }
    }

    if !route.allows_body() {
        if content_length > 0 {
            return None;
        }
        return Some(Request { method, target, body: Vec::new() });
    }

    let mut body = raw[head_end + 4..].to_vec();
    while body.len() < content_length {
        match stream.read(&mut chunk).await {
            Ok(0) => return None,
            Ok(n) => body.extend_from_slice(&chunk[..n]),
            Err(_) => return None,
        }
    }
    body.truncate(content_length);
    Some(Request { method, target, body })
}

async fn handle_snapshot(request: &Request, state: &Arc<Mutex<State>>) -> (u16, Vec<u8>) {
    match request.target.as_str() {
        "/v3/snapshot" => {
            if request.method != "GET" {
                return (405, error_body(405));
            }
            let mut state = state.lock().await;
            state.snapshot_requests += 1;
            let sequence = state.next_sequence;
            state.next_sequence = state.next_sequence.saturating_add(1);
            let mut snapshot = state.snapshot.clone();
            snapshot["sequence"] = serde_json::json!(sequence);
            // 与节点侧 `marshalJSONLine` 一致：紧凑 JSON + 行尾 LF。
            let mut body = serde_json::to_vec(&snapshot).expect("快照可序列化");
            body.push(b'\n');
            (200, body)
        }
        "/healthz" => {
            if request.method != "GET" {
                return (405, error_body(405));
            }
            let state = state.lock().await;
            // /healthz 不推进 sequence，且只看前三位。
            let health = &state.snapshot["health"];
            let unhealthy = ["counter_overflow", "sequence_overflow", "identity_limit_reached"]
                .iter()
                .any(|key| health[*key].as_bool().unwrap_or(false));
            let status = if unhealthy { "unhealthy" } else { "ok" };
            let schema_version = state.snapshot["schema_version"].clone();
            let body = serde_json::to_vec(&serde_json::json!({
                "schema_version": schema_version,
                "status": status,
            }))
            .expect("healthz 可序列化");
            (if unhealthy { 503 } else { 200 }, body)
        }
        // 历史路径恒 404：未同步升级的采集器必须立即失败，
        // 而不是读到一个半兼容的 body（C10）。
        _ => (404, error_body(404)),
    }
}

async fn handle_quota(request: &Request, state: &Arc<Mutex<State>>) -> (u16, Vec<u8>) {
    // 分支顺序严格照 `quota_endpoint.go`：顺序本身是可观察的。
    // 例如「epoch = 0 且 runtime 也不对」返回的是 409 而不是 400，
    // 因为 runtime 比对在 epoch 检查之前。
    if request.target != "/v3/quota" {
        return (404, error_body(404));
    }
    if request.method != "PUT" {
        return (405, error_body(405));
    }

    let mut state = state.lock().await;
    state.quota_requests += 1;

    // 对未知字段失败关闭，与配置面同一纪律。
    let mut deserializer = serde_json::Deserializer::from_slice(&request.body);
    let payload: WireQuotaRequest = match serde::Deserialize::deserialize(&mut deserializer) {
        Ok(value) => value,
        Err(_) => return (400, error_body(400)),
    };
    if deserializer.end().is_err() {
        return (400, error_body(400));
    }

    let expected_schema = state.snapshot["schema_version"].as_u64().unwrap_or(0) as u32;
    if payload.schema_version != expected_schema {
        return (400, error_body(400));
    }
    // node_id 与 runtime_id 必须逐字节相同，否则 409 并整份丢弃。
    if payload.node_id != state.node_id() || payload.runtime_id != state.runtime_id() {
        state.counters.rejected_by_runtime += 1;
        return (409, error_body(409));
    }
    if payload.epoch == 0 {
        return (400, error_body(400));
    }
    // epoch 单调递增，≤ 已接受值即整份丢弃，用于抵抗乱序重投。
    if state.quota_accepted && payload.epoch <= state.quota_epoch {
        state.counters.rejected_by_epoch += 1;
        // 注意：与上面的 runtime 分支**返回同一个错误体**，没有 discriminator。
        return (409, error_body(409));
    }
    for entry in &payload.entries {
        if validate_token(&entry.inbound_tag).is_err() || validate_token(&entry.name).is_err() {
            return (400, error_body(400));
        }
        if entry.remaining_bytes < 0 {
            return (400, error_body(400));
        }
    }

    // 第一遍：全部解析并校验，任何一条不认识就整份拒绝，不写入任何 cell。
    let identities = state.identities();
    let mut unknown = Vec::new();
    for entry in &payload.entries {
        let key = (entry.inbound_tag.clone(), entry.name.clone());
        if !identities.contains(&key) && unknown.len() < 8 {
            unknown.push(format!("{}/{}", entry.inbound_tag, entry.name));
        }
    }
    if !unknown.is_empty() {
        state.counters.rejected_by_unknown += 1;
        let body = serde_json::to_vec(&serde_json::json!({
            "schema_version": expected_schema,
            "error": { "code": 400, "unknown": unknown },
        }))
        .expect("错误体可序列化");
        return (400, body);
    }

    // 第二遍：全量覆盖。不在表内的 lineage 一律解除限额。
    let mut table = BTreeMap::new();
    for entry in &payload.entries {
        table.insert((entry.inbound_tag.clone(), entry.name.clone()), entry.remaining_bytes);
    }
    let applied = table.len();
    state.limited_table = table;
    state.applied_entries = applied;
    state.quota_epoch = payload.epoch;
    state.quota_accepted = true;

    let body = serde_json::to_vec(&serde_json::json!({
        "schema_version": expected_schema,
        "epoch": payload.epoch,
        "applied": applied,
    }))
    .expect("响应可序列化");
    (200, body)
}

#[derive(serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct WireQuotaRequest {
    schema_version: u32,
    node_id: String,
    runtime_id: String,
    epoch: u64,
    entries: Vec<WireQuotaEntry>,
}

#[derive(serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct WireQuotaEntry {
    inbound_tag: String,
    name: String,
    remaining_bytes: i64,
}

fn validate_token(value: &str) -> Result<(), ()> {
    if value.is_empty() || value.len() > 128 {
        return Err(());
    }
    if value.bytes().any(|b| b <= 0x20 || b >= 0x7f) {
        return Err(());
    }
    Ok(())
}

fn error_body(code: u16) -> Vec<u8> {
    // 与节点侧 `newErrorBody` 一致：错误体与快照共用同一个 schema 版本常量。
    serde_json::to_vec(&serde_json::json!({
        "schema_version": proxy_manager::collect::SCHEMA_VERSION,
        "error": { "code": code },
    }))
    .expect("错误体可序列化")
}

fn status_text(status: u16) -> &'static str {
    match status {
        200 => "OK",
        400 => "Bad Request",
        404 => "Not Found",
        405 => "Method Not Allowed",
        408 => "Request Timeout",
        409 => "Conflict",
        413 => "Payload Too Large",
        429 => "Too Many Requests",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        505 => "HTTP Version Not Supported",
        _ => "Unknown",
    }
}

async fn write_response(
    stream: &mut UnixStream,
    status: u16,
    body: Vec<u8>,
    length_delta: Option<i64>,
    truncate: Option<usize>,
) {
    let declared = match length_delta {
        Some(delta) => (body.len() as i64 + delta).max(0) as usize,
        None => body.len(),
    };
    let header = format!(
        "HTTP/1.1 {status} {}\r\nContent-Type: application/json\r\nContent-Length: {declared}\r\nConnection: close\r\n\r\n",
        status_text(status)
    );
    if stream.write_all(header.as_bytes()).await.is_err() {
        return;
    }
    let payload = match truncate {
        Some(keep) => &body[..keep.min(body.len())],
        None => &body[..],
    };
    let _ = stream.write_all(payload).await;
    let _ = stream.flush().await;
    let _ = stream.shutdown().await;
}

fn find(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack.windows(needle.len()).position(|window| window == needle)
}
