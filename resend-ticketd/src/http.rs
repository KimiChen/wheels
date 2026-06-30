use std::sync::Arc;

use anyhow::{Context, Result};
use axum::{
    body::Bytes,
    extract::{Form, Path, Query, State},
    http::{header, HeaderMap, HeaderValue, StatusCode},
    response::{Html, IntoResponse, Redirect, Response},
    routing::{get, post},
    Router,
};
use chrono::{Duration, Utc};
use serde::Deserialize;
use serde_json::Value;

use crate::{
    auth,
    config::Config,
    db::{
        Database, Message, MessageDirection, NewInboundMessage, NewOutboundMessage, TicketStatus,
    },
    resend::ResendClient,
    webhook,
};

#[derive(Clone)]
pub struct AppState {
    inner: Arc<AppStateInner>,
}

struct AppStateInner {
    config: Config,
    database: Database,
    resend: ResendClient,
}

#[derive(Debug, Deserialize)]
struct LoginForm {
    username: String,
    password: String,
}

#[derive(Debug, Deserialize)]
struct ReplyForm {
    csrf: String,
    body: String,
}

#[derive(Debug, Deserialize)]
struct CloseForm {
    csrf: String,
}

#[derive(Debug, Deserialize)]
struct TicketQuery {
    status: Option<String>,
    page: Option<u32>,
}

#[derive(Debug, Deserialize)]
struct ResendWebhook {
    #[serde(default)]
    id: Option<String>,
    #[serde(default, rename = "type")]
    event_type: Option<String>,
    #[serde(default)]
    data: Value,
}

impl AppState {
    pub fn new(config: Config, database: Database, resend: ResendClient) -> Self {
        Self {
            inner: Arc::new(AppStateInner {
                config,
                database,
                resend,
            }),
        }
    }

    fn config(&self) -> &Config {
        &self.inner.config
    }

    fn database(&self) -> &Database {
        &self.inner.database
    }

    fn resend(&self) -> &ResendClient {
        &self.inner.resend
    }
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/health", get(health))
        .route("/healthz", get(health))
        .route("/webhooks/resend", post(resend_webhook))
        .route("/login", get(login_page).post(login))
        .route("/logout", post(logout))
        .route("/", get(root))
        .route("/tickets", get(list_tickets))
        .route("/tickets/:id", get(ticket_detail))
        .route("/tickets/:id/reply", post(reply_ticket))
        .route("/tickets/:id/close", post(close_ticket))
        .with_state(state)
}

async fn health() -> &'static str {
    "ok"
}

async fn root() -> Redirect {
    Redirect::to("/tickets")
}

async fn resend_webhook(
    State(state): State<AppState>,
    headers: HeaderMap,
    body: Bytes,
) -> Response {
    match handle_resend_webhook(state, headers, body).await {
        Ok(()) => StatusCode::NO_CONTENT.into_response(),
        Err(error) => {
            tracing::warn!(%error, "failed to handle Resend webhook");
            (StatusCode::BAD_REQUEST, "invalid webhook").into_response()
        }
    }
}

async fn handle_resend_webhook(state: AppState, headers: HeaderMap, body: Bytes) -> Result<()> {
    let svix_id = required_header(&headers, "svix-id")?;
    let svix_timestamp = required_header(&headers, "svix-timestamp")?;
    let svix_signature = required_header(&headers, "svix-signature")?;
    webhook::verify_svix_signature(
        &state.config().resend_webhook_secret,
        &svix_id,
        &svix_timestamp,
        &svix_signature,
        &body,
    )?;

    let event: ResendWebhook = serde_json::from_slice(&body).context("failed to parse webhook")?;
    let event_id = event.id.unwrap_or(svix_id);
    let event_type = event.event_type.unwrap_or_default();
    if event_type != "email.received" {
        return Ok(());
    }
    if !state
        .database()
        .try_begin_webhook_event(&event_id, &event_type)
        .await?
    {
        return Ok(());
    }

    let email_id =
        extract_email_id(&event.data).context("email.received webhook missing email id")?;
    let received = state
        .resend()
        .retrieve_received_email(&state.config().resend_api_key, &email_id)
        .await?;

    state
        .database()
        .upsert_inbound_message(NewInboundMessage {
            resend_email_id: received.id,
            message_id: received.message_id,
            in_reply_to: received.in_reply_to,
            references: received.references,
            from_email: received.from,
            to_email: received.to,
            subject: received.subject,
            body_text: received.text,
            headers_json: received.headers_json,
            attachments: received.attachments,
        })
        .await?;
    state
        .database()
        .finish_webhook_event(&event_id, "processed")
        .await?;
    Ok(())
}

async fn login_page() -> Html<String> {
    Html(layout("Login", &login_form(None)))
}

async fn login(State(state): State<AppState>, Form(form): Form<LoginForm>) -> Response {
    let valid_username = form.username == state.config().admin_username;
    let valid_password =
        auth::verify_password(&state.config().admin_password_hash, &form.password).unwrap_or(false);

    if !(valid_username && valid_password) {
        return Html(layout(
            "Login",
            &login_form(Some("Invalid username or password")),
        ))
        .into_response();
    }

    let token = auth::generate_token();
    let hash = auth::hash_token(&token);
    if let Err(error) = state
        .database()
        .create_admin_session(&hash, Utc::now() + Duration::hours(12))
        .await
    {
        tracing::warn!(%error, "failed to create session");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    let mut response = Redirect::to("/tickets").into_response();
    response.headers_mut().insert(
        header::SET_COOKIE,
        HeaderValue::from_str(&format!(
            "{}={}; Path=/; Max-Age=43200; HttpOnly; Secure; SameSite=Lax",
            auth::SESSION_COOKIE,
            token
        ))
        .expect("valid cookie header"),
    );
    response
}

async fn logout(State(state): State<AppState>, headers: HeaderMap) -> Response {
    if let Some(token) = session_token(&headers) {
        let _ = state
            .database()
            .delete_admin_session(&auth::hash_token(token))
            .await;
    }
    let mut response = Redirect::to("/login").into_response();
    response.headers_mut().insert(
        header::SET_COOKIE,
        HeaderValue::from_static(
            "resend_ticketd_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Lax",
        ),
    );
    response
}

async fn list_tickets(
    State(state): State<AppState>,
    headers: HeaderMap,
    Query(query): Query<TicketQuery>,
) -> Response {
    let Some(session) = require_session(&state, &headers).await else {
        return Redirect::to("/login").into_response();
    };
    let status = match query.status.as_deref() {
        Some("open") => Some(TicketStatus::Open),
        Some("closed") => Some(TicketStatus::Closed),
        _ => None,
    };
    let tickets = match state
        .database()
        .list_tickets(status, query.page.unwrap_or(1))
        .await
    {
        Ok(tickets) => tickets,
        Err(error) => {
            tracing::warn!(%error, "failed to list tickets");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };
    let csrf = match auth::csrf_token(&session) {
        Ok(csrf) => csrf,
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    };
    Html(layout("Tickets", &tickets_page(&tickets, &csrf))).into_response()
}

async fn ticket_detail(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<i64>,
) -> Response {
    let Some(session) = require_session(&state, &headers).await else {
        return Redirect::to("/login").into_response();
    };
    let ticket = match state.database().get_ticket(id).await {
        Ok(Some(ticket)) => ticket,
        Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(error) => {
            tracing::warn!(%error, "failed to load ticket");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };
    let messages = match state.database().get_ticket_messages(id).await {
        Ok(messages) => messages,
        Err(error) => {
            tracing::warn!(%error, "failed to load ticket messages");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };
    let csrf = match auth::csrf_token(&session) {
        Ok(csrf) => csrf,
        Err(_) => return StatusCode::INTERNAL_SERVER_ERROR.into_response(),
    };
    Html(layout(
        &format!("{} {}", ticket.number, ticket.subject),
        &ticket_page(&ticket, &messages, &csrf),
    ))
    .into_response()
}

#[axum::debug_handler]
async fn reply_ticket(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<i64>,
    Form(form): Form<ReplyForm>,
) -> Response {
    if !valid_csrf(&state, &headers, &form.csrf).await {
        return StatusCode::FORBIDDEN.into_response();
    }
    if form.body.trim().is_empty() {
        return Redirect::to(&format!("/tickets/{id}")).into_response();
    }

    let ticket = match state.database().get_ticket(id).await {
        Ok(Some(ticket)) => ticket,
        Ok(None) => return StatusCode::NOT_FOUND.into_response(),
        Err(error) => {
            tracing::warn!(%error, "failed to load ticket for reply");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };
    let messages = match state.database().get_ticket_messages(id).await {
        Ok(messages) => messages,
        Err(error) => {
            tracing::warn!(%error, "failed to load messages for reply");
            return StatusCode::INTERNAL_SERVER_ERROR.into_response();
        }
    };
    let last_inbound = messages
        .iter()
        .rev()
        .find(|message| message.direction == MessageDirection::Inbound);
    let in_reply_to = last_inbound.and_then(|message| message.message_id.as_deref());
    let references = last_inbound
        .and_then(|message| message.references.as_deref())
        .or(in_reply_to);
    let subject = format!("Re: [{}] {}", ticket.number, ticket.subject);

    let sent = match state
        .resend()
        .send_reply(
            &state.config().resend_api_key,
            &state.config().resend_from,
            &ticket.customer_email,
            &subject,
            &form.body,
            in_reply_to,
            references,
        )
        .await
    {
        Ok(sent) => sent,
        Err(error) => {
            tracing::warn!(%error, "failed to send reply");
            return StatusCode::BAD_GATEWAY.into_response();
        }
    };

    if let Err(error) = state
        .database()
        .insert_outbound_message(
            ticket.id,
            NewOutboundMessage {
                resend_email_id: Some(sent.id),
                message_id: None,
                in_reply_to: in_reply_to.map(ToOwned::to_owned),
                references: references.map(ToOwned::to_owned),
                from_email: state.config().resend_from.clone(),
                to_email: ticket.customer_email,
                subject,
                body_text: form.body,
            },
        )
        .await
    {
        tracing::warn!(%error, "failed to record outbound message");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }

    Redirect::to(&format!("/tickets/{id}")).into_response()
}

async fn close_ticket(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<i64>,
    Form(form): Form<CloseForm>,
) -> Response {
    if !valid_csrf(&state, &headers, &form.csrf).await {
        return StatusCode::FORBIDDEN.into_response();
    }
    if let Err(error) = state.database().close_ticket(id).await {
        tracing::warn!(%error, "failed to close ticket");
        return StatusCode::INTERNAL_SERVER_ERROR.into_response();
    }
    Redirect::to(&format!("/tickets/{id}")).into_response()
}

async fn require_session(state: &AppState, headers: &HeaderMap) -> Option<String> {
    let token = session_token(headers)?;
    let exists = state
        .database()
        .admin_session_exists(&auth::hash_token(token))
        .await
        .ok()?;
    exists.then(|| token.to_string())
}

async fn valid_csrf(state: &AppState, headers: &HeaderMap, csrf: &str) -> bool {
    let Some(token) = require_session(state, headers).await else {
        return false;
    };
    auth::verify_csrf(&token, csrf).unwrap_or(false)
}

fn session_token(headers: &HeaderMap) -> Option<&str> {
    let cookies = headers.get(header::COOKIE)?.to_str().ok()?;
    cookies.split(';').find_map(|cookie| {
        let (name, value) = cookie.trim().split_once('=')?;
        (name == auth::SESSION_COOKIE).then_some(value)
    })
}

fn required_header(headers: &HeaderMap, name: &str) -> Result<String> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(ToOwned::to_owned)
        .with_context(|| format!("{name} header is required"))
}

fn extract_email_id(data: &Value) -> Option<String> {
    [
        "/email_id",
        "/email/id",
        "/id",
        "/object/id",
        "/data/email_id",
        "/data/id",
    ]
    .iter()
    .find_map(|pointer| data.pointer(pointer).and_then(Value::as_str))
    .map(ToOwned::to_owned)
}

fn layout(title: &str, body: &str) -> String {
    format!(
        r#"<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{}</title>
<style>
:root {{ color-scheme: light; font-family: Arial, sans-serif; }}
body {{ margin: 0; background: #f7f8fa; color: #20242a; }}
header {{ display: flex; align-items: center; justify-content: space-between; padding: 14px 24px; background: #17202a; color: white; }}
main {{ max-width: 1080px; margin: 24px auto; padding: 0 18px; }}
a {{ color: #155eef; text-decoration: none; }}
table {{ width: 100%; border-collapse: collapse; background: white; }}
th, td {{ padding: 10px 12px; border-bottom: 1px solid #e3e6ea; text-align: left; vertical-align: top; }}
.panel {{ background: white; border: 1px solid #e3e6ea; border-radius: 8px; padding: 18px; margin-bottom: 16px; }}
.muted {{ color: #667085; }}
.badge {{ display: inline-block; padding: 2px 7px; border-radius: 999px; background: #eef2ff; color: #3538cd; font-size: 12px; }}
input, textarea {{ width: 100%; box-sizing: border-box; padding: 10px; border: 1px solid #cfd4dc; border-radius: 6px; font: inherit; }}
textarea {{ min-height: 180px; resize: vertical; }}
button {{ border: 0; border-radius: 6px; padding: 9px 13px; background: #155eef; color: white; cursor: pointer; }}
button.secondary {{ background: #475467; }}
.actions {{ display: flex; gap: 10px; align-items: center; }}
.message {{ white-space: pre-wrap; background: #fbfcfd; border: 1px solid #e3e6ea; border-radius: 6px; padding: 12px; }}
.error {{ color: #b42318; }}
</style>
</head>
<body>
<header><strong>Resend Ticketd</strong><form method="post" action="/logout"><button class="secondary">Logout</button></form></header>
<main>{}</main>
</body>
</html>"#,
        escape(title),
        body
    )
}

fn login_form(error: Option<&str>) -> String {
    let error = error
        .map(|message| format!(r#"<p class="error">{}</p>"#, escape(message)))
        .unwrap_or_default();
    format!(
        r#"<div class="panel">
<h1>Login</h1>
{error}
<form method="post" action="/login">
<p><label>Username<br><input name="username" autocomplete="username"></label></p>
<p><label>Password<br><input type="password" name="password" autocomplete="current-password"></label></p>
<button>Login</button>
</form>
</div>"#
    )
}

fn tickets_page(tickets: &[crate::db::Ticket], csrf: &str) -> String {
    let mut rows = String::new();
    for ticket in tickets {
        rows.push_str(&format!(
            r#"<tr>
<td><a href="/tickets/{id}">{number}</a></td>
<td>{subject}</td>
<td>{customer}</td>
<td><span class="badge">{status}</span></td>
<td>{created}</td>
<td>{updated}</td>
</tr>"#,
            id = ticket.id,
            number = escape(&ticket.number),
            subject = escape(&ticket.subject),
            customer = escape(&ticket.customer_email),
            status = ticket.status.as_str(),
            created = escape(&ticket.created_at),
            updated = escape(&ticket.updated_at)
        ));
    }
    format!(
        r#"<div class="panel actions">
<a href="/tickets">All</a><a href="/tickets?status=open">Open</a><a href="/tickets?status=closed">Closed</a>
</div>
<table>
<thead><tr><th>Ticket</th><th>Subject</th><th>Customer</th><th>Status</th><th>Created</th><th>Updated</th></tr></thead>
<tbody>{rows}</tbody>
</table>
<form method="post" action="/logout" style="display:none"><input name="csrf" value="{}"></form>"#,
        escape(csrf)
    )
}

fn ticket_page(ticket: &crate::db::Ticket, messages: &[Message], csrf: &str) -> String {
    let mut items = String::new();
    for message in messages {
        items.push_str(&format!(
            r#"<section class="panel">
<p><strong>{direction}</strong> <span class="muted">#{id} ticket {ticket_id} {from} -> {to} at {created}</span></p>
<p>{subject}</p>
<p class="muted">Resend: {resend_id} | Message-ID: {message_id} | In-Reply-To: {in_reply_to}</p>
<details><summary>Headers</summary><pre>{headers}</pre></details>
<div class="message">{body}</div>
</section>"#,
            id = message.id,
            ticket_id = message.ticket_id,
            direction = message.direction.as_str(),
            from = escape(&message.from_email),
            to = escape(&message.to_email),
            created = escape(&message.created_at),
            subject = escape(&message.subject),
            resend_id = escape(message.resend_email_id.as_deref().unwrap_or("-")),
            message_id = escape(message.message_id.as_deref().unwrap_or("-")),
            in_reply_to = escape(message.in_reply_to.as_deref().unwrap_or("-")),
            headers = escape(&message.headers_json),
            body = escape(&message.body_text),
        ));
    }
    format!(
        r#"<div class="panel">
<p><a href="/tickets">Tickets</a></p>
<h1>{number}</h1>
<p>{subject}</p>
<p><span class="badge">{status}</span> <span class="muted">Customer: {customer}</span></p>
<p class="muted">Created: {created} | Updated: {updated} | Closed: {closed}</p>
<form method="post" action="/tickets/{id}/close" class="actions">
<input type="hidden" name="csrf" value="{csrf}">
<button class="secondary">Close</button>
</form>
</div>
{items}
<div class="panel">
<h2>Reply</h2>
<form method="post" action="/tickets/{id}/reply">
<input type="hidden" name="csrf" value="{csrf}">
<p><textarea name="body"></textarea></p>
<button>Send Reply</button>
</form>
</div>"#,
        id = ticket.id,
        csrf = escape(csrf),
        number = escape(&ticket.number),
        subject = escape(&ticket.subject),
        status = ticket.status.as_str(),
        customer = escape(&ticket.customer_email),
        created = escape(&ticket.created_at),
        updated = escape(&ticket.updated_at),
        closed = escape(ticket.closed_at.as_deref().unwrap_or("-")),
        items = items
    )
}

fn escape(input: &str) -> String {
    html_escape::encode_safe(input).to_string()
}
