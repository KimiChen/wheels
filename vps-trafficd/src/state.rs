use crate::counters::InterfaceCounters;
use anyhow::{Context, Result};
use chrono::{DateTime, FixedOffset};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    fs,
    io::Write,
    path::{Path, PathBuf},
};

pub const STATE_VERSION: u32 = 1;

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct State {
    pub version: u32,
    pub cycle_start: DateTime<FixedOffset>,
    pub cycle_end: DateTime<FixedOffset>,
    pub interfaces: BTreeMap<String, InterfaceCounters>,
    pub cycle_rx_bytes: u64,
    pub cycle_tx_bytes: u64,
    pub calibration_rx_offset: i128,
    pub calibration_tx_offset: i128,
    pub updated_at: DateTime<FixedOffset>,
}

impl State {
    pub fn new(
        cycle_start: DateTime<FixedOffset>,
        cycle_end: DateTime<FixedOffset>,
        interfaces: BTreeMap<String, InterfaceCounters>,
        updated_at: DateTime<FixedOffset>,
    ) -> Self {
        Self {
            version: STATE_VERSION,
            cycle_start,
            cycle_end,
            interfaces,
            cycle_rx_bytes: 0,
            cycle_tx_bytes: 0,
            calibration_rx_offset: 0,
            calibration_tx_offset: 0,
            updated_at,
        }
    }

    pub fn reset_cycle(
        &mut self,
        cycle_start: DateTime<FixedOffset>,
        cycle_end: DateTime<FixedOffset>,
        interfaces: BTreeMap<String, InterfaceCounters>,
        updated_at: DateTime<FixedOffset>,
    ) {
        self.cycle_start = cycle_start;
        self.cycle_end = cycle_end;
        self.interfaces = interfaces;
        self.cycle_rx_bytes = 0;
        self.cycle_tx_bytes = 0;
        self.calibration_rx_offset = 0;
        self.calibration_tx_offset = 0;
        self.updated_at = updated_at;
    }
}

pub fn load(path: &Path) -> Result<Option<State>> {
    if !path.exists() {
        return Ok(None);
    }

    let content = fs::read_to_string(path)
        .with_context(|| format!("failed to read state file {}", path.display()))?;
    let state = serde_json::from_str(&content)
        .with_context(|| format!("failed to parse state file {}", path.display()))?;
    Ok(Some(state))
}

pub fn save(path: &Path, state: &State) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("failed to create state directory {}", parent.display()))?;
    }

    let mut tmp_path = temp_path(path);
    let mut suffix = 0_u32;
    while tmp_path.exists() {
        suffix += 1;
        tmp_path = temp_path_with_suffix(path, suffix);
    }

    let content = serde_json::to_vec_pretty(state).context("failed to serialize state")?;
    let mut file = fs::File::create(&tmp_path)
        .with_context(|| format!("failed to create temp state file {}", tmp_path.display()))?;
    file.write_all(&content)
        .with_context(|| format!("failed to write temp state file {}", tmp_path.display()))?;
    file.write_all(b"\n")
        .with_context(|| format!("failed to write temp state file {}", tmp_path.display()))?;
    file.sync_all()
        .with_context(|| format!("failed to sync temp state file {}", tmp_path.display()))?;

    if cfg!(windows) && path.exists() {
        fs::remove_file(path)
            .with_context(|| format!("failed to replace state file {}", path.display()))?;
    }
    fs::rename(&tmp_path, path)
        .with_context(|| format!("failed to install state file {}", path.display()))?;
    Ok(())
}

fn temp_path(path: &Path) -> PathBuf {
    temp_path_with_suffix(path, std::process::id())
}

fn temp_path_with_suffix(path: &Path, suffix: u32) -> PathBuf {
    let file_name = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or("state.json");
    path.with_file_name(format!(".{file_name}.{suffix}.tmp"))
}
