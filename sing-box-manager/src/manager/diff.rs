//! 结构化发布 diff：按 (role, scope_ref) 比较新旧 revision 的 artifact content_sha256（单向摘要，非密）。

use std::collections::{HashMap, HashSet};

use serde_json::{json, Value};
use sqlx::SqlitePool;

use crate::error::Result;
use crate::store::revisions;

pub async fn compute_diff(
    pool: &SqlitePool,
    new_rev_id: &str,
    old_rev_id: Option<&str>,
) -> Result<Value> {
    let new_arts = revisions::list_artifact_meta(pool, new_rev_id).await?;
    let old_arts = match old_rev_id {
        Some(id) => revisions::list_artifact_meta(pool, id).await?,
        None => vec![],
    };
    let old_map: HashMap<(String, String), String> = old_arts
        .iter()
        .map(|a| {
            (
                (a.role.clone(), a.scope_ref.clone()),
                a.content_sha256.clone(),
            )
        })
        .collect();

    let mut changes = Vec::new();
    let mut new_keys = HashSet::new();
    for a in &new_arts {
        let key = (a.role.clone(), a.scope_ref.clone());
        new_keys.insert(key.clone());
        let change = match old_map.get(&key) {
            None => "added",
            Some(s) if *s == a.content_sha256 => "unchanged",
            Some(_) => "changed",
        };
        changes.push(json!({
            "role": a.role, "scope_ref": a.scope_ref, "host_id": a.host_id,
            "change": change, "new_sha": a.content_sha256, "old_sha": old_map.get(&key),
        }));
    }
    for a in &old_arts {
        let key = (a.role.clone(), a.scope_ref.clone());
        if !new_keys.contains(&key) {
            changes.push(json!({
                "role": a.role, "scope_ref": a.scope_ref, "host_id": a.host_id,
                "change": "removed", "old_sha": a.content_sha256,
            }));
        }
    }
    Ok(json!({"new_revision": new_rev_id, "old_revision": old_rev_id, "changes": changes}))
}
