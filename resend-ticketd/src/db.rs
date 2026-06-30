use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use rusqlite::{params, Connection, OptionalExtension};
use std::{path::PathBuf, sync::Arc};
use tokio::sync::Mutex;

#[derive(Clone)]
pub struct Database {
    connection: Arc<Mutex<Connection>>,
}

#[derive(Clone, Debug)]
pub struct Ticket {
    pub id: i64,
    pub number: String,
    pub customer_email: String,
    pub subject: String,
    pub status: TicketStatus,
    pub created_at: String,
    pub updated_at: String,
    pub closed_at: Option<String>,
}

#[derive(Copy, Clone, Debug, Eq, PartialEq)]
pub enum TicketStatus {
    Open,
    Closed,
}

impl TicketStatus {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Open => "open",
            Self::Closed => "closed",
        }
    }

    pub fn parse(value: &str) -> Self {
        match value {
            "closed" => Self::Closed,
            _ => Self::Open,
        }
    }
}

#[derive(Clone, Debug)]
pub struct Message {
    pub id: i64,
    pub ticket_id: i64,
    pub direction: MessageDirection,
    pub resend_email_id: Option<String>,
    pub message_id: Option<String>,
    pub in_reply_to: Option<String>,
    pub references: Option<String>,
    pub from_email: String,
    pub to_email: String,
    pub subject: String,
    pub body_text: String,
    pub headers_json: String,
    pub created_at: String,
}

#[derive(Copy, Clone, Debug, Eq, PartialEq)]
pub enum MessageDirection {
    Inbound,
    Outbound,
}

impl MessageDirection {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Inbound => "inbound",
            Self::Outbound => "outbound",
        }
    }

    pub fn parse(value: &str) -> Self {
        match value {
            "outbound" => Self::Outbound,
            _ => Self::Inbound,
        }
    }
}

#[derive(Clone, Debug)]
pub struct AttachmentRecord {
    pub filename: String,
    pub content_type: Option<String>,
    pub size_bytes: Option<i64>,
    pub resend_attachment_id: Option<String>,
    pub download_url_expires_at: Option<String>,
}

#[derive(Clone, Debug)]
pub struct NewInboundMessage {
    pub resend_email_id: String,
    pub message_id: Option<String>,
    pub in_reply_to: Option<String>,
    pub references: Option<String>,
    pub from_email: String,
    pub to_email: String,
    pub subject: String,
    pub body_text: String,
    pub headers_json: String,
    pub attachments: Vec<AttachmentRecord>,
}

#[derive(Clone, Debug)]
pub struct NewOutboundMessage {
    pub resend_email_id: Option<String>,
    pub message_id: Option<String>,
    pub in_reply_to: Option<String>,
    pub references: Option<String>,
    pub from_email: String,
    pub to_email: String,
    pub subject: String,
    pub body_text: String,
}

impl Database {
    pub fn open(database_url: &str) -> Result<Self> {
        let connection = match sqlite_path_from_url(database_url)? {
            Some(path) => Connection::open(path),
            None => Connection::open_in_memory(),
        }
        .context("failed to open SQLite database")?;
        connection
            .pragma_update(None, "foreign_keys", "ON")
            .context("failed to enable SQLite foreign keys")?;
        Ok(Self {
            connection: Arc::new(Mutex::new(connection)),
        })
    }

    pub async fn migrate(&self) -> Result<()> {
        let connection = self.connection.lock().await;
        connection
            .execute_batch(
                r#"
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tickets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    number TEXT NOT NULL UNIQUE,
    customer_email TEXT NOT NULL,
    subject TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('open', 'closed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    closed_at TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ticket_id INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    resend_email_id TEXT,
    message_id TEXT,
    in_reply_to TEXT,
    references_header TEXT,
    from_email TEXT NOT NULL,
    to_email TEXT NOT NULL,
    subject TEXT NOT NULL,
    body_text TEXT NOT NULL,
    headers_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_message_id ON messages(message_id);
CREATE INDEX IF NOT EXISTS idx_messages_resend_email_id ON messages(resend_email_id);
CREATE INDEX IF NOT EXISTS idx_messages_ticket_id ON messages(ticket_id);

CREATE TABLE IF NOT EXISTS attachments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    filename TEXT NOT NULL,
    content_type TEXT,
    size_bytes INTEGER,
    resend_attachment_id TEXT,
    download_url_expires_at TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS webhook_events (
    event_id TEXT PRIMARY KEY,
    event_type TEXT NOT NULL,
    processing_status TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id_hash TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (1, datetime('now'));
"#,
            )
            .context("failed to migrate SQLite schema")
    }

    pub async fn try_begin_webhook_event(&self, event_id: &str, event_type: &str) -> Result<bool> {
        let now = now_string();
        let connection = self.connection.lock().await;
        let changed = connection
            .execute(
                "INSERT INTO webhook_events (event_id, event_type, processing_status, created_at)
                 VALUES (?1, ?2, 'processing', ?3)
                 ON CONFLICT(event_id) DO UPDATE SET
                    event_type = excluded.event_type,
                    processing_status = 'processing'
                 WHERE webhook_events.processing_status != 'processed'",
                params![event_id, event_type, now],
            )
            .context("failed to insert webhook event")?;
        Ok(changed == 1)
    }

    pub async fn finish_webhook_event(&self, event_id: &str, status: &str) -> Result<()> {
        let connection = self.connection.lock().await;
        connection
            .execute(
                "UPDATE webhook_events SET processing_status = ?1 WHERE event_id = ?2",
                params![status, event_id],
            )
            .context("failed to update webhook event")?;
        Ok(())
    }

    pub async fn upsert_inbound_message(&self, input: NewInboundMessage) -> Result<Ticket> {
        let connection = self.connection.lock().await;
        let tx = connection
            .unchecked_transaction()
            .context("failed to start transaction")?;

        let ticket = match find_ticket_for_message(&tx, &input)? {
            Some(mut ticket) => {
                if ticket.status == TicketStatus::Closed {
                    reopen_ticket(&tx, ticket.id)?;
                    ticket.status = TicketStatus::Open;
                    ticket.closed_at = None;
                }
                touch_ticket(&tx, ticket.id)?;
                ticket
            }
            None => create_ticket(&tx, &input)?,
        };

        let message_id = insert_message(
            &tx,
            ticket.id,
            MessageDirection::Inbound,
            Some(input.resend_email_id.as_str()),
            input.message_id.as_deref().into(),
            input.in_reply_to.as_deref().into(),
            input.references.as_deref().into(),
            &input.from_email,
            &input.to_email,
            &input.subject,
            &input.body_text,
            &input.headers_json,
        )?;

        insert_attachments(&tx, message_id, &input.attachments)?;
        tx.commit().context("failed to commit inbound message")?;
        Ok(ticket)
    }

    pub async fn insert_outbound_message(
        &self,
        ticket_id: i64,
        input: NewOutboundMessage,
    ) -> Result<Message> {
        let connection = self.connection.lock().await;
        let tx = connection
            .unchecked_transaction()
            .context("failed to start transaction")?;
        let message_id = insert_message(
            &tx,
            ticket_id,
            MessageDirection::Outbound,
            input.resend_email_id.as_deref(),
            input.message_id.as_deref(),
            input.in_reply_to.as_deref(),
            input.references.as_deref(),
            &input.from_email,
            &input.to_email,
            &input.subject,
            &input.body_text,
            "{}",
        )?;
        touch_ticket(&tx, ticket_id)?;
        tx.commit().context("failed to commit outbound message")?;
        connection
            .query_row(
                "SELECT id, ticket_id, direction, resend_email_id, message_id, in_reply_to,
                        references_header, from_email, to_email, subject, body_text, headers_json, created_at
                 FROM messages WHERE id = ?1",
                params![message_id],
                message_from_row,
            )
            .context("failed to fetch inserted message")
    }

    pub async fn list_tickets(
        &self,
        status: Option<TicketStatus>,
        page: u32,
    ) -> Result<Vec<Ticket>> {
        let page = page.max(1);
        let offset = i64::from((page - 1) * 50);
        let connection = self.connection.lock().await;
        let mut tickets = Vec::new();

        if let Some(status) = status {
            let mut statement = connection
                .prepare(
                    "SELECT id, number, customer_email, subject, status, created_at, updated_at, closed_at
                     FROM tickets WHERE status = ?1 ORDER BY updated_at DESC LIMIT 50 OFFSET ?2",
                )
                .context("failed to prepare ticket list")?;
            let rows = statement
                .query_map(params![status.as_str(), offset], ticket_from_row)
                .context("failed to list tickets")?;
            for row in rows {
                tickets.push(row.context("failed to read ticket row")?);
            }
        } else {
            let mut statement = connection
                .prepare(
                    "SELECT id, number, customer_email, subject, status, created_at, updated_at, closed_at
                     FROM tickets ORDER BY updated_at DESC LIMIT 50 OFFSET ?1",
                )
                .context("failed to prepare ticket list")?;
            let rows = statement
                .query_map(params![offset], ticket_from_row)
                .context("failed to list tickets")?;
            for row in rows {
                tickets.push(row.context("failed to read ticket row")?);
            }
        }

        Ok(tickets)
    }

    pub async fn get_ticket(&self, id: i64) -> Result<Option<Ticket>> {
        let connection = self.connection.lock().await;
        connection
            .query_row(
                "SELECT id, number, customer_email, subject, status, created_at, updated_at, closed_at
                 FROM tickets WHERE id = ?1",
                params![id],
                ticket_from_row,
            )
            .optional()
            .context("failed to fetch ticket")
    }

    pub async fn get_ticket_messages(&self, ticket_id: i64) -> Result<Vec<Message>> {
        let connection = self.connection.lock().await;
        let mut statement = connection
            .prepare(
                "SELECT id, ticket_id, direction, resend_email_id, message_id, in_reply_to,
                        references_header, from_email, to_email, subject, body_text, headers_json, created_at
                 FROM messages WHERE ticket_id = ?1 ORDER BY id ASC",
            )
            .context("failed to prepare ticket messages")?;
        let rows = statement
            .query_map(params![ticket_id], message_from_row)
            .context("failed to query ticket messages")?;
        let mut messages = Vec::new();
        for row in rows {
            messages.push(row.context("failed to read message row")?);
        }
        Ok(messages)
    }

    pub async fn close_ticket(&self, ticket_id: i64) -> Result<()> {
        let now = now_string();
        let connection = self.connection.lock().await;
        connection
            .execute(
                "UPDATE tickets SET status = 'closed', closed_at = ?1, updated_at = ?1 WHERE id = ?2",
                params![now, ticket_id],
            )
            .context("failed to close ticket")?;
        Ok(())
    }

    pub async fn create_admin_session(
        &self,
        id_hash: &str,
        expires_at: DateTime<Utc>,
    ) -> Result<()> {
        let now = now_string();
        let connection = self.connection.lock().await;
        connection
            .execute(
                "INSERT INTO admin_sessions (id_hash, expires_at, created_at) VALUES (?1, ?2, ?3)",
                params![id_hash, expires_at.to_rfc3339(), now],
            )
            .context("failed to create admin session")?;
        Ok(())
    }

    pub async fn admin_session_exists(&self, id_hash: &str) -> Result<bool> {
        let now = Utc::now().to_rfc3339();
        let connection = self.connection.lock().await;
        let exists: Option<i64> = connection
            .query_row(
                "SELECT 1 FROM admin_sessions WHERE id_hash = ?1 AND expires_at > ?2",
                params![id_hash, now],
                |row| row.get(0),
            )
            .optional()
            .context("failed to check admin session")?;
        Ok(exists.is_some())
    }

    pub async fn delete_admin_session(&self, id_hash: &str) -> Result<()> {
        let connection = self.connection.lock().await;
        connection
            .execute(
                "DELETE FROM admin_sessions WHERE id_hash = ?1",
                params![id_hash],
            )
            .context("failed to delete admin session")?;
        Ok(())
    }
}

fn sqlite_path_from_url(url: &str) -> Result<Option<PathBuf>> {
    if matches!(url, "sqlite::memory:" | "sqlite://:memory:") {
        return Ok(None);
    }
    if let Some(path) = url.strip_prefix("sqlite://") {
        return Ok(Some(PathBuf::from(path)));
    }
    if let Some(path) = url.strip_prefix("sqlite:") {
        return Ok(Some(PathBuf::from(path)));
    }
    anyhow::bail!("DATABASE_URL must use sqlite:// or sqlite: format")
}

fn find_ticket_for_message(
    tx: &rusqlite::Transaction<'_>,
    input: &NewInboundMessage,
) -> Result<Option<Ticket>> {
    if let Some(number) = ticket_number_from_subject(&input.subject) {
        if let Some(ticket) = tx
            .query_row(
                "SELECT id, number, customer_email, subject, status, created_at, updated_at, closed_at
                 FROM tickets WHERE number = ?1",
                params![number],
                ticket_from_row,
            )
            .optional()
            .context("failed to find ticket by number")?
        {
            return Ok(Some(ticket));
        }
    }

    let mut candidates = Vec::new();
    if let Some(value) = input.in_reply_to.as_deref() {
        candidates.push(value.trim().to_string());
    }
    if let Some(value) = input.references.as_deref() {
        candidates.extend(
            value
                .split_whitespace()
                .map(str::trim)
                .filter(|item| !item.is_empty())
                .map(ToOwned::to_owned),
        );
    }
    if let Some(value) = input.message_id.as_deref() {
        candidates.push(value.trim().to_string());
    }

    for candidate in candidates {
        if let Some(ticket) = tx
            .query_row(
                "SELECT t.id, t.number, t.customer_email, t.subject, t.status, t.created_at, t.updated_at, t.closed_at
                 FROM tickets t
                 JOIN messages m ON m.ticket_id = t.id
                 WHERE m.message_id = ?1
                 ORDER BY m.id DESC LIMIT 1",
                params![candidate],
                ticket_from_row,
            )
            .optional()
            .context("failed to find ticket by message headers")?
        {
            return Ok(Some(ticket));
        }
    }

    Ok(None)
}

fn create_ticket(tx: &rusqlite::Transaction<'_>, input: &NewInboundMessage) -> Result<Ticket> {
    let now = now_string();
    tx.execute(
        "INSERT INTO tickets (number, customer_email, subject, status, created_at, updated_at)
         VALUES ('pending', ?1, ?2, 'open', ?3, ?3)",
        params![input.from_email, input.subject, now],
    )
    .context("failed to create ticket")?;
    let id = tx.last_insert_rowid();
    let number = format!("TKT-{id:06}");
    tx.execute(
        "UPDATE tickets SET number = ?1 WHERE id = ?2",
        params![number, id],
    )
    .context("failed to assign ticket number")?;
    Ok(Ticket {
        id,
        number,
        customer_email: input.from_email.clone(),
        subject: input.subject.clone(),
        status: TicketStatus::Open,
        created_at: now.clone(),
        updated_at: now,
        closed_at: None,
    })
}

fn insert_message(
    tx: &rusqlite::Transaction<'_>,
    ticket_id: i64,
    direction: MessageDirection,
    resend_email_id: Option<&str>,
    message_id: Option<&str>,
    in_reply_to: Option<&str>,
    references: Option<&str>,
    from_email: &str,
    to_email: &str,
    subject: &str,
    body_text: &str,
    headers_json: &str,
) -> Result<i64> {
    let now = now_string();
    tx.execute(
        "INSERT INTO messages (
            ticket_id, direction, resend_email_id, message_id, in_reply_to, references_header,
            from_email, to_email, subject, body_text, headers_json, created_at
         ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12)",
        params![
            ticket_id,
            direction.as_str(),
            resend_email_id,
            message_id,
            in_reply_to,
            references,
            from_email,
            to_email,
            subject,
            body_text,
            headers_json,
            now,
        ],
    )
    .context("failed to insert message")?;
    Ok(tx.last_insert_rowid())
}

fn insert_attachments(
    tx: &rusqlite::Transaction<'_>,
    message_id: i64,
    attachments: &[AttachmentRecord],
) -> Result<()> {
    let now = now_string();
    for attachment in attachments {
        tx.execute(
            "INSERT INTO attachments (
                message_id, filename, content_type, size_bytes, resend_attachment_id,
                download_url_expires_at, created_at
             ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
            params![
                message_id,
                attachment.filename,
                attachment.content_type,
                attachment.size_bytes,
                attachment.resend_attachment_id,
                attachment.download_url_expires_at,
                now,
            ],
        )
        .context("failed to insert attachment")?;
    }
    Ok(())
}

fn reopen_ticket(tx: &rusqlite::Transaction<'_>, ticket_id: i64) -> Result<()> {
    let now = now_string();
    tx.execute(
        "UPDATE tickets SET status = 'open', closed_at = NULL, updated_at = ?1 WHERE id = ?2",
        params![now, ticket_id],
    )
    .context("failed to reopen ticket")?;
    Ok(())
}

fn touch_ticket(tx: &rusqlite::Transaction<'_>, ticket_id: i64) -> Result<()> {
    let now = now_string();
    tx.execute(
        "UPDATE tickets SET updated_at = ?1 WHERE id = ?2",
        params![now, ticket_id],
    )
    .context("failed to update ticket timestamp")?;
    Ok(())
}

fn ticket_from_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<Ticket> {
    Ok(Ticket {
        id: row.get(0)?,
        number: row.get(1)?,
        customer_email: row.get(2)?,
        subject: row.get(3)?,
        status: TicketStatus::parse(row.get::<_, String>(4)?.as_str()),
        created_at: row.get(5)?,
        updated_at: row.get(6)?,
        closed_at: row.get(7)?,
    })
}

fn message_from_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<Message> {
    Ok(Message {
        id: row.get(0)?,
        ticket_id: row.get(1)?,
        direction: MessageDirection::parse(row.get::<_, String>(2)?.as_str()),
        resend_email_id: row.get(3)?,
        message_id: row.get(4)?,
        in_reply_to: row.get(5)?,
        references: row.get(6)?,
        from_email: row.get(7)?,
        to_email: row.get(8)?,
        subject: row.get(9)?,
        body_text: row.get(10)?,
        headers_json: row.get(11)?,
        created_at: row.get(12)?,
    })
}

fn ticket_number_from_subject(subject: &str) -> Option<String> {
    static PATTERN: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let regex = PATTERN.get_or_init(|| regex::Regex::new(r"\[(TKT-\d{6})\]").unwrap());
    regex
        .captures(subject)
        .and_then(|captures| captures.get(1))
        .map(|found| found.as_str().to_string())
}

fn now_string() -> String {
    Utc::now().to_rfc3339()
}
