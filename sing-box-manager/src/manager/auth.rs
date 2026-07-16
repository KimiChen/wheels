//! 管理面认证域：角色/认证主体、Argon2id 密码原语、Cookie 工具，以及 setup/login/logout/whoami/reauth
//! 与管理员/会话/审计管理 handler。认证边界仅此管理面（9736 的 /api），公开订阅 /sub 与探针不经此。

use std::sync::OnceLock;

use argon2::password_hash::{
    rand_core::OsRng, PasswordHash, PasswordHasher, PasswordVerifier, SaltString,
};
use argon2::Argon2;
use axum::extract::{Path, State};
use axum::http::{header, HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde::Deserialize;
use serde_json::json;

use crate::error::{AppError, ErrorCode, Result};
use crate::manager::http::AppState;
use crate::store::{admins, audit, sessions};

type ApiResult = std::result::Result<Response, AppError>;

// ---------- 角色 ----------

/// 三角色，序数用于 min-role 比较。
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Role {
    Readonly = 0,
    Operator = 1,
    Admin = 2,
}

impl Role {
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "readonly" => Some(Role::Readonly),
            "operator" => Some(Role::Operator),
            "admin" => Some(Role::Admin),
            _ => None,
        }
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Role::Readonly => "readonly",
            Role::Operator => "operator",
            Role::Admin => "admin",
        }
    }
}

/// 认证主体，由 session_mw 放进 request extensions，handler 经 `Extension<AuthCtx>` 取。
#[derive(Debug, Clone)]
pub struct AuthCtx {
    pub admin_id: String,
    pub username: String,
    pub role: Role,
    pub session_id_hash: String,
    pub last_reauth_at: Option<i64>,
}

// ---------- Argon2id 密码原语 ----------

pub fn hash_password(pw: &str) -> Result<String> {
    let salt = SaltString::generate(&mut OsRng);
    let hash = Argon2::default()
        .hash_password(pw.as_bytes(), &salt)
        .map_err(|e| AppError::new(ErrorCode::Internal, format!("密码哈希失败: {e}")))?;
    Ok(hash.to_string())
}

pub fn verify_password(hash: &str, pw: &str) -> bool {
    match PasswordHash::new(hash) {
        Ok(parsed) => Argon2::default()
            .verify_password(pw.as_bytes(), &parsed)
            .is_ok(),
        Err(_) => false,
    }
}

/// 抗时序：用户不存在时也跑一次等价开销的假校验。
fn dummy_verify(pw: &str) {
    static DUMMY: OnceLock<String> = OnceLock::new();
    let h = DUMMY.get_or_init(|| hash_password("__nonexistent_account__").unwrap_or_default());
    let _ = verify_password(h, pw);
}

// ---------- Cookie 工具 ----------

/// 从 Cookie 头取指定 cookie 值。
pub fn parse_cookie(headers: &HeaderMap, name: &str) -> Option<String> {
    let raw = headers.get(header::COOKIE)?.to_str().ok()?;
    for part in raw.split(';') {
        if let Some((k, v)) = part.trim().split_once('=') {
            if k == name {
                return Some(v.to_string());
            }
        }
    }
    None
}

fn set_cookie(name: &str, value: &str, max_age: i64, secure: bool) -> String {
    let mut s = format!("{name}={value}; Path=/; HttpOnly; SameSite=Strict; Max-Age={max_age}");
    if secure {
        s.push_str("; Secure");
    }
    s
}

fn json_cookie(status: StatusCode, body: serde_json::Value, cookie: Option<String>) -> Response {
    let mut resp = (status, Json(body)).into_response();
    if let Some(c) = cookie {
        if let Ok(v) = header::HeaderValue::from_str(&c) {
            resp.headers_mut().append(header::SET_COOKIE, v);
        }
    }
    resp
}

fn client_ip(headers: &HeaderMap) -> Option<String> {
    headers
        .get("x-forwarded-for")
        .and_then(|v| v.to_str().ok())
        .and_then(|s| s.split(',').next())
        .map(|s| s.trim().to_string())
}

fn actor(username: &str) -> String {
    format!("admin:{username}")
}

// ---------- handlers ----------

#[derive(Deserialize)]
pub struct SetupReq {
    username: String,
    password: String,
}

/// POST /api/auth/setup：仅当尚无任何管理员时放行，创建首个 admin；否则 409。公开面（无会话）。
pub async fn setup(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(r): Json<SetupReq>,
) -> ApiResult {
    validate_password(&r.password)?;
    let hash = hash_password(&r.password)?;
    // 原子首建：并发 setup 竞态下只有一个成功，其余 409（审查 B）。
    let Some(id) = admins::create_if_none(&st.pool, &r.username, &hash, "admin").await? else {
        return Err(AppError::new(
            ErrorCode::Conflict,
            "管理员已存在，setup 已关闭",
        ));
    };
    let _ = audit::record(
        &st.pool,
        Some(&actor(&r.username)),
        "admin.setup",
        Some("admin"),
        Some(&id),
        None,
        None,
    )
    .await;
    let _ = client_ip(&headers);
    Ok((
        StatusCode::CREATED,
        Json(json!({"id": id, "username": r.username})),
    )
        .into_response())
}

#[derive(Deserialize)]
pub struct LoginReq {
    username: String,
    password: String,
}

/// POST /api/auth/login：校验密码 + 节流 → 建会话 + Set-Cookie + 返回 csrf_token。公开面。
pub async fn login(
    State(st): State<AppState>,
    headers: HeaderMap,
    Json(r): Json<LoginReq>,
) -> ApiResult {
    let cfg = &st.auth;
    let now = crate::store::now_unix();
    let admin = admins::get_by_username(&st.pool, &r.username).await?;
    let Some(admin) = admin else {
        dummy_verify(&r.password); // 抗时序枚举
        return Err(unauthorized_login());
    };
    // **先**验证密码（无论后续 disabled/locked），使各分支耗时均匀 → 无时序枚举（审查 A）。
    let pw_ok = verify_password(&admin.password_hash, &r.password);
    let locked = admin.locked_until.map(|t| t > now).unwrap_or(false);
    // 停用/锁定/密码错一律**同一**响应文案与状态（不泄露账号是否存在/停用/锁定），审计里才区分原因。
    if admin.disabled || locked {
        let reason = if admin.disabled {
            "admin.login.disabled"
        } else {
            "admin.login.locked"
        };
        let _ = audit::record(
            &st.pool,
            Some(&actor(&r.username)),
            reason,
            Some("admin"),
            Some(&admin.id),
            None,
            None,
        )
        .await;
        return Err(unauthorized_login());
    }
    if !pw_ok {
        admins::note_login_fail(&st.pool, &admin.id, cfg.lock_threshold, cfg.lock_secs).await?;
        let _ = audit::record(
            &st.pool,
            Some(&actor(&r.username)),
            "admin.login.fail",
            Some("admin"),
            Some(&admin.id),
            None,
            None,
        )
        .await;
        return Err(unauthorized_login());
    }
    admins::note_login_ok(&st.pool, &admin.id).await?;
    let ip = client_ip(&headers);
    let ua = headers
        .get(header::USER_AGENT)
        .and_then(|v| v.to_str().ok());
    let (sid, csrf) = sessions::create(
        &st.pool,
        &admin.id,
        cfg.idle_ttl_secs,
        cfg.absolute_ttl_secs,
        ip.as_deref(),
        ua,
    )
    .await?;
    let _ = audit::record(
        &st.pool,
        Some(&actor(&r.username)),
        "admin.login",
        Some("admin"),
        Some(&admin.id),
        None,
        None,
    )
    .await;
    let cookie = set_cookie(
        &cfg.cookie_name,
        &sid,
        cfg.absolute_ttl_secs,
        cfg.secure_cookie,
    );
    Ok(json_cookie(
        StatusCode::OK,
        json!({"role": admin.role, "csrf_token": csrf, "username": admin.username}),
        Some(cookie),
    ))
}

/// POST /api/auth/logout：吊销当前会话 + 清 Cookie。
pub async fn logout(
    State(st): State<AppState>,
    axum::Extension(ctx): axum::Extension<AuthCtx>,
) -> ApiResult {
    sessions::revoke(&st.pool, &ctx.session_id_hash).await?;
    let _ = audit::record(
        &st.pool,
        Some(&actor(&ctx.username)),
        "admin.logout",
        Some("admin"),
        Some(&ctx.admin_id),
        None,
        None,
    )
    .await;
    let cookie = set_cookie(&st.auth.cookie_name, "", 0, st.auth.secure_cookie);
    Ok(json_cookie(
        StatusCode::OK,
        json!({"ok": true}),
        Some(cookie),
    ))
}

/// GET /api/auth/whoami：当前身份 + 新鲜 csrf 状态（reauth 有效期）。
pub async fn whoami(
    State(st): State<AppState>,
    axum::Extension(ctx): axum::Extension<AuthCtx>,
) -> ApiResult {
    let now = crate::store::now_unix();
    let reauth_valid_until = ctx
        .last_reauth_at
        .map(|t| t + st.auth.reauth_window_secs)
        .filter(|&u| u > now);
    Ok(Json(json!({
        "username": ctx.username,
        "role": ctx.role.as_str(),
        "reauth_valid_until": reauth_valid_until,
    }))
    .into_response())
}

#[derive(Deserialize)]
pub struct ReauthReq {
    password: String,
}

/// POST /api/auth/reauth：用当前身份重验密码 → 盖 last_reauth_at（供敏感操作门）。
pub async fn reauth(
    State(st): State<AppState>,
    axum::Extension(ctx): axum::Extension<AuthCtx>,
    Json(r): Json<ReauthReq>,
) -> ApiResult {
    let admin = admins::get_by_id(&st.pool, &ctx.admin_id)
        .await?
        .ok_or_else(|| AppError::new(ErrorCode::Unauthorized, "会话无效"))?;
    if !verify_password(&admin.password_hash, &r.password) {
        let _ = audit::record(
            &st.pool,
            Some(&actor(&ctx.username)),
            "admin.reauth.fail",
            Some("admin"),
            Some(&ctx.admin_id),
            None,
            None,
        )
        .await;
        return Err(AppError::new(ErrorCode::Unauthorized, "密码错误"));
    }
    sessions::stamp_reauth(&st.pool, &ctx.session_id_hash, crate::store::now_unix()).await?;
    let _ = audit::record(
        &st.pool,
        Some(&actor(&ctx.username)),
        "admin.reauth",
        Some("admin"),
        Some(&ctx.admin_id),
        None,
        None,
    )
    .await;
    Ok(Json(json!({"ok": true})).into_response())
}

// ---------- 管理员/会话/审计管理（admin 角色 + reauth）----------

pub async fn list_admins(State(st): State<AppState>) -> ApiResult {
    Ok(Json(json!({"admins": admins::list(&st.pool).await?})).into_response())
}

#[derive(Deserialize)]
pub struct CreateAdminReq {
    username: String,
    password: String,
    role: String,
}

pub async fn create_admin(
    State(st): State<AppState>,
    axum::Extension(ctx): axum::Extension<AuthCtx>,
    Json(r): Json<CreateAdminReq>,
) -> ApiResult {
    if Role::parse(&r.role).is_none() {
        return Err(AppError::new(ErrorCode::Validation, "非法角色"));
    }
    validate_password(&r.password)?;
    let hash = hash_password(&r.password)?;
    let id = admins::create(&st.pool, &r.username, &hash, &r.role).await?;
    let _ = audit::record(
        &st.pool,
        Some(&actor(&ctx.username)),
        "admin.create",
        Some("admin"),
        Some(&id),
        None,
        Some(&format!("role={}", r.role)),
    )
    .await;
    Ok((StatusCode::CREATED, Json(json!({"id": id}))).into_response())
}

#[derive(Deserialize)]
pub struct UpdateAdminReq {
    role: Option<String>,
    disabled: Option<bool>,
    password: Option<String>,
}

pub async fn update_admin(
    State(st): State<AppState>,
    axum::Extension(ctx): axum::Extension<AuthCtx>,
    Path(id): Path<String>,
    Json(r): Json<UpdateAdminReq>,
) -> ApiResult {
    if admins::get_by_id(&st.pool, &id).await?.is_none() {
        return Err(AppError::new(ErrorCode::NotFound, "管理员不存在"));
    }
    if let Some(role) = &r.role {
        if Role::parse(role).is_none() {
            return Err(AppError::new(ErrorCode::Validation, "非法角色"));
        }
        admins::set_role(&st.pool, &id, role).await?;
        let _ = audit::record(
            &st.pool,
            Some(&actor(&ctx.username)),
            "admin.role.change",
            Some("admin"),
            Some(&id),
            None,
            Some(&format!("role={role}")),
        )
        .await;
    }
    if let Some(dis) = r.disabled {
        admins::set_disabled(&st.pool, &id, dis).await?;
        if dis {
            sessions::revoke_all_for_admin(&st.pool, &id).await?;
        }
        let _ = audit::record(
            &st.pool,
            Some(&actor(&ctx.username)),
            "admin.disable",
            Some("admin"),
            Some(&id),
            None,
            Some(&format!("disabled={dis}")),
        )
        .await;
    }
    if let Some(pw) = &r.password {
        validate_password(pw)?;
        let hash = hash_password(pw)?;
        admins::set_password(&st.pool, &id, &hash).await?;
        sessions::revoke_all_for_admin(&st.pool, &id).await?; // 改密作废该管理员全部会话
        let _ = audit::record(
            &st.pool,
            Some(&actor(&ctx.username)),
            "admin.password.change",
            Some("admin"),
            Some(&id),
            None,
            None,
        )
        .await;
    }
    Ok(Json(json!({"ok": true})).into_response())
}

pub async fn list_sessions(State(st): State<AppState>, Path(admin_id): Path<String>) -> ApiResult {
    Ok(
        Json(json!({"sessions": sessions::list_for_admin(&st.pool, &admin_id).await?}))
            .into_response(),
    )
}

#[derive(Deserialize)]
pub struct AuditQuery {
    limit: Option<i64>,
}

pub async fn list_audit(
    State(st): State<AppState>,
    q: axum::extract::Query<AuditQuery>,
) -> ApiResult {
    let limit = q.limit.unwrap_or(200);
    Ok(Json(json!({"audit": audit::list_recent(&st.pool, limit).await?})).into_response())
}

// ---------- helpers ----------

fn unauthorized_login() -> AppError {
    // 恒定文案，不区分「用户不存在 vs 密码错」。
    AppError::new(ErrorCode::Unauthorized, "用户名或密码错误")
}

fn validate_password(pw: &str) -> Result<()> {
    if pw.chars().count() < 12 {
        return Err(AppError::new(ErrorCode::Validation, "密码至少 12 字符"));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn argon2_hash_verify_roundtrip() {
        let h = hash_password("correct horse battery staple").unwrap();
        assert!(h.starts_with("$argon2id$"));
        assert!(verify_password(&h, "correct horse battery staple"));
        assert!(!verify_password(&h, "wrong password"));
        assert!(!verify_password("not-a-phc-string", "x"));
    }

    #[test]
    fn role_ordering_and_parse() {
        assert!(Role::Admin > Role::Operator && Role::Operator > Role::Readonly);
        assert_eq!(Role::parse("operator"), Some(Role::Operator));
        assert_eq!(Role::parse("bogus"), None);
    }
}
