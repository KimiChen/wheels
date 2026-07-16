//! Host 与能力领域类型。

use serde::Serialize;

/// Host 能力（对齐 `host_capabilities.capability` CHECK）。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Capability {
    Manage,
    Entry,
    Node,
}

impl Capability {
    pub fn as_str(self) -> &'static str {
        match self {
            Capability::Manage => "manage",
            Capability::Entry => "entry",
            Capability::Node => "node",
        }
    }
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "manage" => Some(Capability::Manage),
            "entry" => Some(Capability::Entry),
            "node" => Some(Capability::Node),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct Host {
    pub id: String,
    pub name: String,
    pub note: Option<String>,
    pub created_at: i64,
    pub updated_at: i64,
}
