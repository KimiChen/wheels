use anyhow::{Context, Result};
use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::db::AttachmentRecord;

const RESEND_BASE_URL: &str = "https://api.resend.com";

#[derive(Clone, Default)]
pub struct ResendClient {
    http: reqwest::Client,
}

#[derive(Clone, Debug)]
pub struct ReceivedEmail {
    pub id: String,
    pub from: String,
    pub to: String,
    pub subject: String,
    pub text: String,
    pub message_id: Option<String>,
    pub in_reply_to: Option<String>,
    pub references: Option<String>,
    pub headers_json: String,
    pub attachments: Vec<AttachmentRecord>,
}

#[derive(Clone, Debug)]
pub struct SentEmail {
    pub id: String,
}

#[derive(Debug, Deserialize)]
struct ReceivedEmailResponse {
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    from: Option<EmailAddress>,
    #[serde(default)]
    to: Vec<EmailAddress>,
    #[serde(default)]
    subject: Option<String>,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    html: Option<String>,
    #[serde(default)]
    headers: Value,
    #[serde(default)]
    attachments: Vec<AttachmentResponse>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
enum EmailAddress {
    String(String),
    Object {
        #[serde(default)]
        email: String,
        #[serde(default)]
        name: Option<String>,
    },
}

#[derive(Debug, Deserialize)]
struct AttachmentResponse {
    #[serde(default, alias = "filename")]
    name: Option<String>,
    #[serde(default, alias = "content_type")]
    content_type: Option<String>,
    #[serde(default, alias = "size")]
    size_bytes: Option<i64>,
    #[serde(default, alias = "id")]
    attachment_id: Option<String>,
    #[serde(default, alias = "download_url_expires_at")]
    expires_at: Option<String>,
}

#[derive(Serialize)]
struct SendEmailRequest<'a> {
    from: &'a str,
    to: Vec<&'a str>,
    subject: &'a str,
    text: &'a str,
    #[serde(rename = "reply_to", skip_serializing_if = "Option::is_none")]
    reply_to: Option<Vec<&'a str>>,
    #[serde(rename = "headers", skip_serializing_if = "Option::is_none")]
    headers: Option<SendHeaders<'a>>,
}

#[derive(Serialize)]
struct SendHeaders<'a> {
    #[serde(rename = "In-Reply-To", skip_serializing_if = "Option::is_none")]
    in_reply_to: Option<&'a str>,
    #[serde(rename = "References", skip_serializing_if = "Option::is_none")]
    references: Option<&'a str>,
}

#[derive(Deserialize)]
struct SendEmailResponse {
    id: String,
}

impl ResendClient {
    pub fn new() -> Self {
        Self {
            http: reqwest::Client::new(),
        }
    }

    pub async fn retrieve_received_email(
        &self,
        api_key: &str,
        email_id: &str,
    ) -> Result<ReceivedEmail> {
        let response = self
            .http
            .get(received_email_url(email_id))
            .bearer_auth(api_key)
            .send()
            .await
            .context("failed to call Resend retrieve received email API")?;

        ensure_success(response.status(), "retrieve received email")?;
        let raw = response
            .json::<ReceivedEmailResponse>()
            .await
            .context("failed to parse Resend received email response")?;

        let headers_json =
            serde_json::to_string(&raw.headers).context("failed to serialize email headers")?;
        let message_id = header_value(&raw.headers, "Message-ID");
        let in_reply_to = header_value(&raw.headers, "In-Reply-To");
        let references = header_value(&raw.headers, "References");
        let text = raw
            .text
            .filter(|text| !text.trim().is_empty())
            .or_else(|| raw.html.map(|html| html_to_text(&html)))
            .unwrap_or_default();

        Ok(ReceivedEmail {
            id: raw.id.unwrap_or_else(|| email_id.to_string()),
            from: raw
                .from
                .map(|value| value.into_string())
                .unwrap_or_default(),
            to: raw
                .to
                .first()
                .map(|value| value.clone().into_string())
                .unwrap_or_default(),
            subject: raw.subject.unwrap_or_else(|| "(no subject)".to_string()),
            text,
            message_id,
            in_reply_to,
            references,
            headers_json,
            attachments: raw
                .attachments
                .into_iter()
                .map(|attachment| AttachmentRecord {
                    filename: attachment.name.unwrap_or_else(|| "attachment".to_string()),
                    content_type: attachment.content_type,
                    size_bytes: attachment.size_bytes,
                    resend_attachment_id: attachment.attachment_id,
                    download_url_expires_at: attachment.expires_at,
                })
                .collect(),
        })
    }

    pub async fn send_reply(
        &self,
        api_key: &str,
        from: &str,
        to: &str,
        subject: &str,
        text: &str,
        in_reply_to: Option<&str>,
        references: Option<&str>,
    ) -> Result<SentEmail> {
        let body = SendEmailRequest {
            from,
            to: vec![to],
            subject,
            text,
            reply_to: None,
            headers: Some(SendHeaders {
                in_reply_to,
                references,
            }),
        };
        let response = self
            .http
            .post(format!("{RESEND_BASE_URL}/emails"))
            .bearer_auth(api_key)
            .json(&body)
            .send()
            .await
            .context("failed to call Resend send email API")?;

        ensure_success(response.status(), "send email")?;
        let raw = response
            .json::<SendEmailResponse>()
            .await
            .context("failed to parse Resend send email response")?;
        Ok(SentEmail { id: raw.id })
    }
}

impl EmailAddress {
    fn into_string(self) -> String {
        match self {
            Self::String(value) => value,
            Self::Object { email, name } => match name {
                Some(name) if !name.is_empty() => format!("{name} <{email}>"),
                _ => email,
            },
        }
    }
}

fn received_email_url(email_id: &str) -> String {
    format!("{RESEND_BASE_URL}/emails/receiving/{email_id}")
}

fn ensure_success(status: StatusCode, action: &str) -> Result<()> {
    if status.is_success() {
        Ok(())
    } else {
        anyhow::bail!("Resend {action} API returned HTTP {status}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn received_email_url_uses_receiving_endpoint() {
        assert_eq!(
            received_email_url("email_123"),
            "https://api.resend.com/emails/receiving/email_123"
        );
    }
}

fn header_value(headers: &Value, name: &str) -> Option<String> {
    let needle = name.to_ascii_lowercase();
    match headers {
        Value::Object(map) => map.iter().find_map(|(key, value)| {
            if key.to_ascii_lowercase() == needle {
                value.as_str().map(ToOwned::to_owned)
            } else {
                None
            }
        }),
        Value::Array(items) => items.iter().find_map(|item| {
            let key = item
                .get("name")
                .or_else(|| item.get("key"))
                .and_then(Value::as_str)?;
            if key.to_ascii_lowercase() == needle {
                item.get("value")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned)
            } else {
                None
            }
        }),
        _ => None,
    }
}

fn html_to_text(html: &str) -> String {
    let stripped = regex::Regex::new(r"(?is)<(script|style)[^>]*>.*?</\1>")
        .expect("valid regex")
        .replace_all(html, "");
    let stripped = regex::Regex::new(r"(?s)<[^>]+>")
        .expect("valid regex")
        .replace_all(&stripped, "\n");
    html_escape::decode_html_entities(&stripped).to_string()
}
