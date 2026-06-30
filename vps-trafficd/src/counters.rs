use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::{fs, path::Path};

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct InterfaceCounters {
    pub rx_bytes: u64,
    pub tx_bytes: u64,
}

pub fn read_interface_counters(sysfs_root: &Path, iface: &str) -> Result<InterfaceCounters> {
    let stats_dir = sysfs_root.join(iface).join("statistics");
    let rx_bytes = read_counter(&stats_dir.join("rx_bytes"))
        .with_context(|| format!("failed to read rx_bytes for interface {iface}"))?;
    let tx_bytes = read_counter(&stats_dir.join("tx_bytes"))
        .with_context(|| format!("failed to read tx_bytes for interface {iface}"))?;
    Ok(InterfaceCounters { rx_bytes, tx_bytes })
}

fn read_counter(path: &Path) -> Result<u64> {
    let content =
        fs::read_to_string(path).with_context(|| format!("failed to read {}", path.display()))?;
    content
        .trim()
        .parse::<u64>()
        .with_context(|| format!("failed to parse {}", path.display()))
}
