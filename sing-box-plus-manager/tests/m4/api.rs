//! API 层、会话与越权的验收用例（README §6、§4.9、§4.11）。
//!
//! 用 `tower::oneshot` 直接驱动 router：不起网络、不开端口，
//! 但走的是**真实的提取器、中间件与 handler**，不是在测试里重写一遍分发。

use axum::body::Body;
use axum::http::{header, Request, StatusCode};
use proxy_manager::api::session::{self, Role};
use serde_json::Value;

use crate::harness::Api;

/// 遍历 JSON，断言每个以 `_bytes` 结尾的键都是**字符串**（C3）。
///
/// 这是 C3 在 API 边界的机械化：一个返回裸数字的端点会在这里被抓住，
/// 而不是等到前端把它喂进 `Number` 之后才在某个大流量用户身上出错。
fn assert_bytes_are_strings(value: &Value, path: &str) {
    match value {
        Value::Object(map) => {
            for (key, child) in map {
                let child_path = format!("{path}.{key}");
                if key.ends_with("_bytes") && !child.is_null() {
                    assert!(
                        child.is_string(),
                        "{child_path} 必须是十进制字符串（C3），实际 {child}"
                    );
                }
                assert_bytes_are_strings(child, &child_path);
            }
        }
        Value::Array(items) => {
            for (index, child) in items.iter().enumerate() {
                assert_bytes_are_strings(child, &format!("{path}[{index}]"));
            }
        }
        _ => {}
    }
}

fn error_code(body: &Value) -> &str {
    body["error"]["code"].as_str().unwrap_or("<缺少 code>")
}

// ============ 认证 ============

#[tokio::test]
async fn 没有会话一律401() {
    let api = Api::new().await;
    for path in ["/api/v1/me", "/api/v1/nodes", "/api/v1/users", "/api/v1/settings/quota"] {
        let (status, body, _) = api.get(path, None).await;
        assert_eq!(status, StatusCode::UNAUTHORIZED, "{path}");
        assert_eq!(error_code(&body), "unauthenticated", "{path}");
    }
    // 健康探针不需要认证——它们不返回任何业务数据。
    assert_eq!(api.get("/healthz", None).await.0, StatusCode::OK);
    assert_eq!(api.get("/readyz", None).await.0, StatusCode::OK);
}

/// 伪造的会话 cookie 在 **HMAC 证明** 那一步就被挡掉。
#[tokio::test]
async fn 伪造的会话证明被拒() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let (id, proof) = admin.cookie.split_once('.').unwrap();

    for forged in [
        format!("{id}.{}", "0".repeat(64)),    // 证明是编的
        format!("{}.{proof}", "f".repeat(64)), // ID 换了，证明对不上
        id.to_string(),                        // 没有证明
        format!("{id}."),                      // 空证明
        format!("nothex.{proof}"),             // ID 不是 hex
    ] {
        let mut actor_cookie = admin.cookie.clone();
        actor_cookie.clone_from(&forged);
        let request = Request::builder()
            .uri("/api/v1/me")
            .header(header::COOKIE, format!("{}={}", session::SESSION_COOKIE, forged))
            .body(Body::empty())
            .unwrap();
        let (status, body, _) = api.send(request).await;
        assert_eq!(status, StatusCode::UNAUTHORIZED, "{forged}");
        assert_eq!(error_code(&body), "unauthenticated");
    }
}

/// **本地撤销在每请求检查，下一次请求立即生效**（D13）。
#[tokio::test]
async fn 吊销后下一个请求立即失效() {
    let api = Api::new().await;
    let admin = api.admin().await;
    assert_eq!(api.get("/api/v1/me", Some(&admin)).await.0, StatusCode::OK);

    session::revoke(&api.store, admin.session_pk).await.unwrap();

    let (status, body, _) = api.get("/api/v1/me", Some(&admin)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    // 与「从来没登录」分开：前者是「你本来有」。
    assert_eq!(error_code(&body), "session_revoked");
}

/// 成员事实过期且无法刷新 → 拒绝，**不允许用过期事实继续放行**（D13）。
#[tokio::test]
async fn 成员事实过期时拒绝受保护请求() {
    let api = Api::new().await;
    // IM provider 的会话才依赖成员事实。
    let actor = api.user("im-user", Role::User, "feishu").await;
    assert_eq!(api.get("/api/v1/me", Some(&actor)).await.0, StatusCode::OK);

    session::expire_member_fact(&api.store, actor.session_pk).await.unwrap();
    let (status, body, _) = api.get("/api/v1/me", Some(&actor)).await;
    assert_eq!(status, StatusCode::UNAUTHORIZED);
    assert_eq!(error_code(&body), "member_fact_stale");
    assert!(body["error"]["message"].as_str().unwrap().contains("认证依赖不可用"));
}

/// 反向证据：`local`（break-glass）不依赖上游，不受成员事实影响。
#[tokio::test]
async fn local会话不依赖成员事实() {
    let api = Api::new().await;
    let actor = api.user("break-glass", Role::Admin, "local").await;
    session::expire_member_fact(&api.store, actor.session_pk).await.unwrap();
    assert_eq!(api.get("/api/v1/me", Some(&actor)).await.0, StatusCode::OK);
}

/// 停用账号仍拒绝访问，**不等同于默认普通用户**。
#[tokio::test]
async fn 停用账号被拒而不是降级为普通用户() {
    let api = Api::new().await;
    let actor = api.admin().await;
    let mut txn = api.store.begin_immediate().await.unwrap();
    sqlx::query("UPDATE users SET status = 'disabled' WHERE user_id = ?")
        .bind(actor.user_id)
        .execute(txn.conn())
        .await
        .unwrap();
    txn.commit().await.unwrap();

    let (status, body, _) = api.get("/api/v1/me", Some(&actor)).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(error_code(&body), "forbidden");
}

// ============ 越权 ============

/// **越权必须在服务端被拒绝，响应里不能出现任何被保护的数据。**
#[tokio::test]
async fn 普通用户访问管理端点被服务端拒绝() {
    let api = Api::new().await;
    let user = api.plain_user().await;

    for path in [
        "/api/v1/nodes",
        "/api/v1/identities",
        "/api/v1/users",
        "/api/v1/settings/quota",
        "/api/v1/alerts",
    ] {
        let (status, body, _) = api.get(path, Some(&user)).await;
        assert_eq!(status, StatusCode::FORBIDDEN, "{path}");
        assert_eq!(error_code(&body), "forbidden", "{path}");
        // 「拿不到的数据根本不该出现在响应里，而不是发过来再藏起来」。
        let text = body.to_string();
        for leaked in ["node_id", "login_name", "monthly_bytes", "acceptance_criteria"] {
            assert!(!text.contains(leaked), "{path} 的 403 响应里泄露了 {leaked}");
        }
    }
}

/// **角色不由客户端选择或声明。**
#[tokio::test]
async fn 客户端声明角色无效() {
    let api = Api::new().await;
    let user = api.plain_user().await;
    let request = Request::builder()
        .uri("/api/v1/nodes?role=admin")
        .header(header::COOKIE, format!("{}={}", session::SESSION_COOKIE, user.cookie))
        .header("x-role", "admin")
        .header("x-user-id", "1")
        .body(Body::empty())
        .unwrap();
    let (status, _, _) = api.send(request).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "角色只来自服务端求值");
}

/// 个人端点的目标固定取会话主体，管理员也看不到别人的个人数据入口。
#[tokio::test]
async fn 个人端点只返回自己的数据() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let user = api.plain_user().await;

    let (_, body, _) = api.get("/api/v1/me", Some(&user)).await;
    assert_eq!(body["user_id"].as_i64(), Some(user.user_id));
    let (_, body, _) = api.get("/api/v1/me", Some(&admin)).await;
    assert_eq!(body["user_id"].as_i64(), Some(admin.user_id), "管理员的 /me 也是他自己");
}

// ============ CSRF ============

#[tokio::test]
async fn 写操作缺少或错配csrf一律拒绝() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let body = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2000000000000" });

    // 缺 header
    let (status, resp, _) =
        api.put_json("/api/v1/settings/quota", &admin, body.clone(), None, None).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(error_code(&resp), "csrf_invalid");

    // header 与 cookie 不一致
    let (status, resp, _) = api
        .put_json("/api/v1/settings/quota", &admin, body.clone(), Some(&"a".repeat(64)), None)
        .await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(error_code(&resp), "csrf_invalid");

    // 拿**另一个会话**的 CSRF token：header 与 cookie 都得换，才谈得上「一致」
    let other = api.user("other", Role::Admin, "local").await;
    let request = Request::builder()
        .uri("/api/v1/settings/quota")
        .method("PUT")
        .header(
            header::COOKIE,
            format!(
                "{}={}; {}={}",
                session::SESSION_COOKIE,
                admin.cookie,
                session::CSRF_COOKIE,
                other.csrf
            ),
        )
        .header(session::CSRF_HEADER, &other.csrf)
        .header(header::CONTENT_TYPE, "application/json")
        .body(Body::from(body.to_string()))
        .unwrap();
    let (status, resp, _) = api.send(request).await;
    assert_eq!(status, StatusCode::FORBIDDEN, "别的会话的 token 不算数");
    assert_eq!(error_code(&resp), "csrf_invalid");

    // 反向证据：带对的就能过。
    let (status, _, _) =
        api.put_json("/api/v1/settings/quota", &admin, body, Some(&admin.csrf), None).await;
    assert_eq!(status, StatusCode::ACCEPTED);
}

// ============ 乐观并发与 dry_run ============

#[tokio::test]
async fn if_match不符返回412() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let body = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2000000000000" });
    api.put_json("/api/v1/settings/quota", &admin, body.clone(), Some(&admin.csrf), None).await;

    // 当前 revision 是 1；拿 0 去提交必须被拒。
    let (status, resp, _) = api
        .put_json("/api/v1/settings/quota", &admin, body.clone(), Some(&admin.csrf), Some("0"))
        .await;
    assert_eq!(status, StatusCode::PRECONDITION_FAILED);
    assert_eq!(error_code(&resp), "revision_mismatch");
    assert_eq!(resp["error"]["detail"]["current_revision"].as_i64(), Some(1));

    // 拿对的就能过。
    let (status, _, _) =
        api.put_json("/api/v1/settings/quota", &admin, body, Some(&admin.csrf), Some("1")).await;
    assert_eq!(status, StatusCode::ACCEPTED);
}

/// `dry_run` **只返回预览，不改设置、不写下发任务**。
#[tokio::test]
async fn dry_run不改设置() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let set = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2000000000000" });
    api.put_json("/api/v1/settings/quota", &admin, set, Some(&admin.csrf), None).await;

    let preview = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "1", "dry_run": true });
    let (status, body, _) =
        api.put_json("/api/v1/settings/quota", &admin, preview, Some(&admin.csrf), None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body["dry_run"], true);
    assert!(body["is_reduction"].as_bool().unwrap());
    // **用量核验时刻**必须在。
    assert!(!body["usage_verified_at"].as_str().unwrap().is_empty());
    assert_bytes_are_strings(&body, "preview");

    // 设置没变。
    let (_, current, _) = api.get("/api/v1/settings/quota", Some(&admin)).await;
    let normal = current["groups"]
        .as_array()
        .unwrap()
        .iter()
        .find(|g| g["group_name"] == "normal")
        .expect("normal 档");
    assert_eq!(normal["monthly_bytes"], "2000000000000");
    assert_eq!(current["revision"].as_i64(), Some(1));
}

/// 调低未确认 → 422；调高不要求确认。
#[tokio::test]
async fn 调低必须确认而调高不要求() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let set = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2000000000000" });
    api.put_json("/api/v1/settings/quota", &admin, set, Some(&admin.csrf), None).await;

    let lower =
        serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "1" });
    let (status, resp, _) = api
        .put_json("/api/v1/settings/quota", &admin, lower.clone(), Some(&admin.csrf), None)
        .await;
    assert_eq!(status, StatusCode::UNPROCESSABLE_ENTITY);
    assert_eq!(error_code(&resp), "confirmation_required");

    let confirmed = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "1", "confirm": true });
    let (status, _, _) =
        api.put_json("/api/v1/settings/quota", &admin, confirmed, Some(&admin.csrf), None).await;
    assert_eq!(status, StatusCode::ACCEPTED);

    // 调高不要求。
    let higher = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2147483648" });
    let (status, _, _) =
        api.put_json("/api/v1/settings/quota", &admin, higher, Some(&admin.csrf), None).await;
    assert_eq!(status, StatusCode::ACCEPTED);
}

/// 202 的语义：**「立即生效」不表示节点已确认收到新表。**
#[tokio::test]
async fn 提交返回202且不声称全部已生效() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let body = serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "2000000000000" });
    let (status, resp, _) =
        api.put_json("/api/v1/settings/quota", &admin, body, Some(&admin.csrf), None).await;
    assert_eq!(status, StatusCode::ACCEPTED);
    assert_eq!(resp["revision"].as_i64(), Some(1));
    assert!(resp["note"].as_str().unwrap().contains("节点待生效"));
    // 调高不会让已断的连接自己回来——这句必须写在响应里，否则会被当成没生效。
    assert!(resp["note"].as_str().unwrap().contains("重连"));

    let (_, current, _) = api.get("/api/v1/settings/quota", Some(&admin)).await;
    assert_eq!(current["fully_applied"], false, "没有节点确认过，不能说全部已生效");
    // **不提供超发上界字段**（D20、R2）。
    assert!(current.get("overcommit_upper_bound").is_none());
    assert!(current["overcommit_note"].as_str().unwrap().contains("可能超发"));
}

// ============ 口径与形状 ============

/// 所有字节字段都是十进制字符串（C3）。
#[tokio::test]
async fn 字节字段一律是十进制字符串() {
    let api = Api::new().await;
    let admin = api.admin().await;
    api.put_json(
        "/api/v1/settings/quota",
        &admin,
        // i64::MAX：额度的上界（C31 —— wire 上 remaining_bytes 是非负 int64）。
        // 仍然远超 IEEE754 双精度能精确表示的范围，足以验证全链路不走浮点。
        serde_json::json!({ "scope": "group", "group_name": "normal", "monthly_bytes": "9223372036854775807" }),
        Some(&admin.csrf),
        None,
    )
    .await;

    for path in [
        "/api/v1/me",
        "/api/v1/me/usage",
        "/api/v1/nodes",
        "/api/v1/users",
        "/api/v1/settings/quota",
    ] {
        let (status, body, _) = api.get(path, Some(&admin)).await;
        assert_eq!(status, StatusCode::OK, "{path}");
        assert_bytes_are_strings(&body, path);
    }

    // 上界也要原样回来，不能被 JSON 数字精度削掉。
    let (_, body, _) = api.get("/api/v1/settings/quota", Some(&admin)).await;
    let normal = body["groups"]
        .as_array()
        .unwrap()
        .iter()
        .find(|g| g["group_name"] == "normal")
        .expect("normal 档");
    assert_eq!(normal["monthly_bytes"], "9223372036854775807");
}

/// 超过 i64::MAX 的额度必须被**拒绝**，不能被静默截断。
///
/// 这条原本在 `tests/m0/config_gate.rs`，钉的是配置里的 `quota.monthly_bytes`。
/// 分档之后那个字段已删除，纪律的载体变成 `quota_groups` 的定宽文本列与它的
/// i64 上界 CHECK——分配那一层对超界值做的是静默截断，把上界写进库，
/// 等于把一次静默截断换成一次写入失败。
#[tokio::test]
async fn 超过i64上界的额度被拒而不是截断() {
    let api = Api::new().await;
    let admin = api.admin().await;

    for bad in ["9223372036854775808", "18446744073709551615"] {
        let (status, _, _) = api
            .put_json(
                "/api/v1/settings/quota",
                &admin,
                serde_json::json!({
                    "scope": "group", "group_name": "normal", "monthly_bytes": bad, "confirm": true
                }),
                Some(&admin.csrf),
                None,
            )
            .await;
        assert_ne!(status, StatusCode::ACCEPTED, "额度 {bad} 超出 i64::MAX，必须被拒");
    }

    // 反向证据：档位没有被改成一个截断值，仍是种子值。
    let (_, body, _) = api.get("/api/v1/settings/quota", Some(&admin)).await;
    let normal = body["groups"]
        .as_array()
        .unwrap()
        .iter()
        .find(|g| g["group_name"] == "normal")
        .expect("normal 档");
    // 二进制 TiB（2026-09-20 由十进制改来）：客户端按 GiB 显示，
    // 十进制的 1 TB 在用户屏幕上是 931 G，与档位名对不上。
    assert_eq!(normal["monthly_bytes"], "1099511627776", "普通档应当还是种下的 1 TiB");
}

/// 错误对象形状固定，不随端点变化。
#[tokio::test]
async fn 错误对象形状固定() {
    let api = Api::new().await;
    let user = api.plain_user().await;
    for (path, actor) in [("/api/v1/nodes", Some(&user)), ("/api/v1/me", None)] {
        let (_, body, _) = api.get(path, actor).await;
        let error = &body["error"];
        assert!(error["code"].is_string(), "{path} 缺 code");
        assert!(error["message"].is_string(), "{path} 缺 message");
        assert!(error["detail"].is_object(), "{path} 缺 detail");
        assert_eq!(body.as_object().unwrap().len(), 1, "{path} 顶层只能有 error");
    }
}

/// 登录态响应一律 `private, no-store` + `Vary: Cookie`。
#[tokio::test]
async fn 登录态响应不进共享缓存() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let (_, _, headers) = api.get("/api/v1/me", Some(&admin)).await;
    assert_eq!(headers[header::CACHE_CONTROL], "private, no-store");
    assert_eq!(headers[header::VARY], "Cookie");
}

/// 分页用 `cursor` + `limit`，**不用 offset**。
#[tokio::test]
async fn 分页用游标不用offset() {
    let api = Api::new().await;
    let admin = api.admin().await;
    for index in 0..5 {
        api.user(&format!("u{index}"), Role::User, "local").await;
    }

    let (_, page1, _) = api.get("/api/v1/users?limit=2", Some(&admin)).await;
    assert_eq!(page1["users"].as_array().unwrap().len(), 2);
    let cursor = page1["next_cursor"].as_str().expect("应当有 next_cursor");

    let (_, page2, _) =
        api.get(&format!("/api/v1/users?limit=2&cursor={cursor}"), Some(&admin)).await;
    let first_ids: Vec<i64> =
        page1["users"].as_array().unwrap().iter().map(|u| u["user_id"].as_i64().unwrap()).collect();
    let second_ids: Vec<i64> =
        page2["users"].as_array().unwrap().iter().map(|u| u["user_id"].as_i64().unwrap()).collect();
    assert!(second_ids.iter().all(|id| !first_ids.contains(id)), "两页不得重叠");

    // 非法 limit 被拒。
    for bad in ["0", "-1", "100000"] {
        let (status, body, _) = api.get(&format!("/api/v1/users?limit={bad}"), Some(&admin)).await;
        assert_eq!(status, StatusCode::BAD_REQUEST, "limit={bad}");
        assert_eq!(error_code(&body), "invalid_request");
    }
}

/// **管理员真的调一次 `/api/v1/identities`，并核对字段。**
///
/// 这条是补上的。原来这个端点只被「普通用户调了被拒」那条用例覆盖过，
/// **200 那条路径从来没有人走过**——而它里面的 SQL 选了一个
/// `runtime_identities` 上根本不存在的列（`i.user_id`，被
/// `schema/02_runtime_lifecycle.sql` 显式删掉了，注释写着「它是错的地方」）。
/// `sqlx::query` 是运行时检查，所以它编译得过、部署得上、一调就 500。
///
/// 这个洞漏到线上的直接原因就是缺这条用例：一个「只测被拒、不测放行」的
/// 端点用例，证明的是权限，不是它能用。
#[tokio::test]
async fn 管理员取得到计费身份列表() {
    let api = Api::new().await;
    api.seed_claimable_pool(&["node-a"], &["slot-01", "slot-02"]).await;
    let admin = api.admin().await;

    let (status, body, _) = api.get("/api/v1/identities", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK, "{body}");

    let rows = body["identities"].as_array().expect("要有 identities");
    assert_eq!(rows.len(), 2, "夹具种了两个身份：{body}");

    // 六元组要齐：generation 属于协议键，不能因为恒为 1 就省略。
    let row = &rows[0];
    for key in [
        "id",
        "node_id",
        "runtime_id",
        "inbound_tag",
        "inbound_generation",
        "identity_name",
        "identity_generation",
        "active",
    ] {
        assert!(!row[key].is_null(), "{key} 缺失或为 null：{row}");
    }
    // 归属来自 identity_routes 的 LEFT JOIN；没人认领时是 null，**不是报错**。
    assert!(row.get("user_id").is_some(), "user_id 这个键要在：{row}");
    assert_eq!(row["node_id"], "node-a");
    assert_eq!(row["inbound_generation"], "1", "定宽文本要解成十进制字符串");
}

/// 尚未启用的能力回 `audit_not_enabled`，**不是 404**。
///
/// 404 会被读成「打错了」，而真相是「这个能力还没开」——
/// 没有记录可看和不让你看是两回事（D18）。
///
/// 订阅曾经也在这一条里；M6 之后它是真的了，
/// 「没配 `[subscription]`」那一支由 `tests/m5` 的 `没配订阅时明确回未启用` 钉住。
#[tokio::test]
async fn 未启用的能力明确回未启用() {
    let api = Api::without_audit().await;
    let admin = api.admin().await;
    for path in ["/api/v1/me/audit/access", "/api/v1/users/1/audit/access"] {
        let (status, body, _) = api.get(path, Some(&admin)).await;
        assert_eq!(status, StatusCode::CONFLICT, "{path}");
        assert_eq!(error_code(&body), "audit_not_enabled", "{path}");
    }
}

/// 告警页接的是**已经落库的事实**：四项判据置顶。
#[tokio::test]
async fn 告警页把四项判据放第一行() {
    let api = Api::new().await;
    let admin = api.admin().await;
    let (status, body, _) = api.get("/api/v1/alerts", Some(&admin)).await;
    assert_eq!(status, StatusCode::OK);
    let criteria = &body["acceptance_criteria"];
    for key in ["counter_regression", "unknown_runtime", "stale_sequence", "unhealthy_accounted"] {
        assert!(criteria[key].is_i64(), "缺 {key}");
    }
    assert_eq!(criteria["all_zero"], true);
    // §4.10 的规则引擎与 IM 通道属于 M5，这里如实标注。
    assert_eq!(body["rule_engine_enabled"], false);
}
