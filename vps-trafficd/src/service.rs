use crate::{
    billing,
    config::{BillingMode, Config},
    counters::{read_interface_counters, InterfaceCounters},
    state::{self, State, STATE_VERSION},
};
use anyhow::{bail, Context, Result};
use chrono::{DateTime, FixedOffset, Utc};
use serde::Serialize;
use std::{
    collections::BTreeMap,
    fs::{self, OpenOptions},
    path::{Path, PathBuf},
};

#[derive(Clone)]
pub struct TrafficService {
    config: Config,
    sysfs_root: PathBuf,
}

#[derive(Debug, Serialize)]
pub struct TrafficSnapshot {
    pub node_id: String,
    pub cycle_start: DateTime<FixedOffset>,
    pub cycle_end: DateTime<FixedOffset>,
    pub quota_bytes: u64,
    pub billing_mode: &'static str,
    pub rx_bytes: u64,
    pub tx_bytes: u64,
    pub used_bytes: u64,
    pub remaining_bytes: u64,
    pub usage_ratio: f64,
    pub updated_at: DateTime<FixedOffset>,
}

impl TrafficService {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            sysfs_root: PathBuf::from("/sys/class/net"),
        }
    }

    #[cfg(test)]
    pub fn with_sysfs_root(config: Config, sysfs_root: PathBuf) -> Self {
        Self { config, sysfs_root }
    }

    pub fn check(&self) -> Result<()> {
        self.config.validate()?;
        self.read_interfaces()?;
        self.check_state_path()?;
        Ok(())
    }

    pub fn auth_token(&self) -> &str {
        &self.config.auth_token
    }

    pub fn ensure_state(&self) -> Result<()> {
        self.update_state().map(|_| ())
    }

    pub fn snapshot(&self) -> Result<TrafficSnapshot> {
        let state = self.update_state()?;
        let rx_bytes = apply_offset(state.cycle_rx_bytes, state.calibration_rx_offset);
        let tx_bytes = apply_offset(state.cycle_tx_bytes, state.calibration_tx_offset);
        let used_bytes = match self.config.billing_mode {
            BillingMode::Rx => rx_bytes,
            BillingMode::Tx => tx_bytes,
            BillingMode::Total => rx_bytes.saturating_add(tx_bytes),
        };
        let remaining_bytes = self.config.quota_bytes.saturating_sub(used_bytes);
        let usage_ratio = used_bytes as f64 / self.config.quota_bytes as f64;

        Ok(TrafficSnapshot {
            node_id: self.config.node_id.clone(),
            cycle_start: state.cycle_start,
            cycle_end: state.cycle_end,
            quota_bytes: self.config.quota_bytes,
            billing_mode: billing_mode_name(self.config.billing_mode),
            rx_bytes,
            tx_bytes,
            used_bytes,
            remaining_bytes,
            usage_ratio,
            updated_at: state.updated_at,
        })
    }

    pub fn calibrate(&self, rx: u64, tx: u64) -> Result<()> {
        let mut state = self.update_state()?;
        state.calibration_rx_offset = i128::from(rx) - i128::from(state.cycle_rx_bytes);
        state.calibration_tx_offset = i128::from(tx) - i128::from(state.cycle_tx_bytes);
        state.updated_at = self.now();
        state::save(&self.config.state_path, &state)
    }

    fn update_state(&self) -> Result<State> {
        let now = self.now();
        let cycle =
            billing::current_cycle(self.config.cycle_anchor, self.config.cycle_months, now)?;
        let current = self.read_interfaces()?;

        let mut state = match state::load(&self.config.state_path)? {
            Some(mut loaded) => {
                if loaded.version != STATE_VERSION {
                    bail!("unsupported state version {}", loaded.version);
                }
                if loaded.cycle_start != cycle.start || loaded.cycle_end != cycle.end {
                    loaded.reset_cycle(cycle.start, cycle.end, current, now);
                    state::save(&self.config.state_path, &loaded)?;
                    return Ok(loaded);
                }
                loaded
            }
            None => {
                let created = State::new(cycle.start, cycle.end, current, now);
                state::save(&self.config.state_path, &created)?;
                return Ok(created);
            }
        };

        for (iface, counters) in &current {
            match state.interfaces.get(iface) {
                Some(previous) => {
                    if counters.rx_bytes >= previous.rx_bytes {
                        state.cycle_rx_bytes = state
                            .cycle_rx_bytes
                            .saturating_add(counters.rx_bytes - previous.rx_bytes);
                    }
                    if counters.tx_bytes >= previous.tx_bytes {
                        state.cycle_tx_bytes = state
                            .cycle_tx_bytes
                            .saturating_add(counters.tx_bytes - previous.tx_bytes);
                    }
                }
                None => {
                    tracing::info!(%iface, "tracking new network interface");
                }
            }
        }

        state.interfaces = current;
        state.updated_at = now;
        state::save(&self.config.state_path, &state)?;
        Ok(state)
    }

    fn read_interfaces(&self) -> Result<BTreeMap<String, InterfaceCounters>> {
        let mut counters = BTreeMap::new();
        for iface in &self.config.interfaces {
            let iface_counters = read_interface_counters(&self.sysfs_root, iface)?;
            counters.insert(iface.clone(), iface_counters);
        }
        Ok(counters)
    }

    fn check_state_path(&self) -> Result<()> {
        let parent = self
            .config
            .state_path
            .parent()
            .unwrap_or_else(|| Path::new("."));
        fs::create_dir_all(parent)
            .with_context(|| format!("failed to create state directory {}", parent.display()))?;

        let probe_path = parent.join(".vps-trafficd-write-check");
        OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&probe_path)
            .with_context(|| format!("state directory is not writable: {}", parent.display()))?;
        let _ = fs::remove_file(probe_path);
        Ok(())
    }

    fn now(&self) -> DateTime<FixedOffset> {
        Utc::now().with_timezone(self.config.cycle_anchor.offset())
    }
}

fn apply_offset(value: u64, offset: i128) -> u64 {
    let adjusted = i128::from(value) + offset;
    if adjusted <= 0 {
        0
    } else {
        adjusted.min(i128::from(u64::MAX)) as u64
    }
}

fn billing_mode_name(mode: BillingMode) -> &'static str {
    match mode {
        BillingMode::Rx => "rx",
        BillingMode::Tx => "tx",
        BillingMode::Total => "total",
    }
}
