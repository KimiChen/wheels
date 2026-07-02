use crate::{
    billing,
    config::{BillingMode, Config},
    counters::{read_interface_counters, InterfaceCounters},
    state::{self, State, STATE_VERSION},
};
use anyhow::{bail, Context, Result};
use chrono::{DateTime, FixedOffset, Utc};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    fs::{self, OpenOptions},
    path::{Path, PathBuf},
    sync::RwLock,
};

pub struct TrafficService {
    config: RwLock<Config>,
    config_path: PathBuf,
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
    pub updated_at: DateTime<FixedOffset>,
}

#[derive(Debug, Serialize)]
pub struct ConfigSnapshot {
    pub listen_addr: String,
    pub interfaces: Vec<String>,
    pub node_id: String,
    pub quota_bytes: u64,
    pub billing_mode: &'static str,
    pub traffic_cycle_anchor: DateTime<FixedOffset>,
    pub traffic_cycle_months: u32,
    pub state_path: String,
}

#[derive(Debug, Deserialize)]
pub struct ConfigUpdate {
    pub traffic_cycle_anchor: DateTime<FixedOffset>,
    pub traffic_cycle_months: u32,
    pub quota_bytes: u64,
    pub billing_mode: BillingMode,
    #[serde(default)]
    pub current_cycle_used_bytes: Option<u64>,
}

impl TrafficService {
    pub fn new(config: Config, config_path: impl Into<PathBuf>) -> Self {
        Self {
            config: RwLock::new(config),
            config_path: config_path.into(),
            sysfs_root: PathBuf::from("/sys/class/net"),
        }
    }

    #[cfg(test)]
    pub fn with_sysfs_root(config: Config, config_path: PathBuf, sysfs_root: PathBuf) -> Self {
        Self {
            config: RwLock::new(config),
            config_path,
            sysfs_root,
        }
    }

    pub fn check(&self) -> Result<()> {
        let config = self.config()?;
        config.validate()?;
        self.read_interfaces(&config)?;
        self.check_state_path(&config)?;
        Ok(())
    }

    pub fn auth_token(&self) -> Result<String> {
        Ok(self.config()?.auth_token)
    }

    pub fn ensure_state(&self) -> Result<()> {
        let config = self.config()?;
        self.update_state(&config).map(|_| ())
    }

    pub fn snapshot(&self) -> Result<TrafficSnapshot> {
        let config = self.config()?;
        let state = self.update_state(&config)?;
        let rx_bytes = apply_offset(state.cycle_rx_bytes, state.calibration_rx_offset);
        let tx_bytes = apply_offset(state.cycle_tx_bytes, state.calibration_tx_offset);
        let used_bytes = match config.billing_mode {
            BillingMode::Rx => rx_bytes,
            BillingMode::Tx => tx_bytes,
            BillingMode::Total => rx_bytes.saturating_add(tx_bytes),
            BillingMode::Max => rx_bytes.max(tx_bytes),
        };
        let remaining_bytes = config.quota_bytes.saturating_sub(used_bytes);

        Ok(TrafficSnapshot {
            node_id: config.node_id.clone(),
            cycle_start: state.cycle_start,
            cycle_end: state.cycle_end,
            quota_bytes: config.quota_bytes,
            billing_mode: billing_mode_name(config.billing_mode),
            rx_bytes,
            tx_bytes,
            used_bytes,
            remaining_bytes,
            updated_at: state.updated_at,
        })
    }

    pub fn calibrate(&self, rx: u64, tx: u64) -> Result<()> {
        let config = self.config()?;
        let mut state = self.update_state(&config)?;
        state.calibration_rx_offset = i128::from(rx) - i128::from(state.cycle_rx_bytes);
        state.calibration_tx_offset = i128::from(tx) - i128::from(state.cycle_tx_bytes);
        state.updated_at = self.now(&config);
        state::save(&config.state_path, &state)
    }

    pub fn config_snapshot(&self) -> Result<ConfigSnapshot> {
        Ok(config_snapshot(&self.config()?))
    }

    pub fn update_config(&self, update: ConfigUpdate) -> Result<ConfigSnapshot> {
        if update.traffic_cycle_months == 0 {
            bail!("traffic_cycle_months must be greater than zero");
        }
        if update.quota_bytes == 0 {
            bail!("quota_bytes must be greater than zero");
        }

        let mut next = self.config()?;
        next.cycle_anchor = update.traffic_cycle_anchor;
        next.cycle_months = update.traffic_cycle_months;
        next.quota_bytes = update.quota_bytes;
        next.billing_mode = update.billing_mode;
        let current_cycle_used_bytes = update.current_cycle_used_bytes;
        next.validate()?;
        self.read_interfaces(&next)?;
        self.check_state_path(&next)?;
        next.save_commented(&self.config_path)?;

        {
            let mut guard = self
                .config
                .write()
                .map_err(|_| anyhow::anyhow!("config lock poisoned"))?;
            *guard = next.clone();
        }
        let mut state = self.update_state(&next)?;
        if let Some(target_used) = current_cycle_used_bytes {
            apply_used_calibration(&next, &mut state, target_used);
            state.updated_at = self.now(&next);
            state::save(&next.state_path, &state)?;
        }
        Ok(config_snapshot(&next))
    }

    fn update_state(&self, config: &Config) -> Result<State> {
        let now = self.now(config);
        let cycle = billing::current_cycle(config.cycle_anchor, config.cycle_months, now)?;
        let current = self.read_interfaces(config)?;

        let mut state = match state::load(&config.state_path)? {
            Some(mut loaded) => {
                if loaded.version != STATE_VERSION {
                    bail!("unsupported state version {}", loaded.version);
                }
                if loaded.cycle_start != cycle.start || loaded.cycle_end != cycle.end {
                    loaded.reset_cycle(cycle.start, cycle.end, current, now);
                    state::save(&config.state_path, &loaded)?;
                    return Ok(loaded);
                }
                loaded
            }
            None => {
                let created = State::new(cycle.start, cycle.end, current, now);
                state::save(&config.state_path, &created)?;
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
        state::save(&config.state_path, &state)?;
        Ok(state)
    }

    fn read_interfaces(&self, config: &Config) -> Result<BTreeMap<String, InterfaceCounters>> {
        let mut counters = BTreeMap::new();
        for iface in &config.interfaces {
            let iface_counters = read_interface_counters(&self.sysfs_root, iface)?;
            counters.insert(iface.clone(), iface_counters);
        }
        Ok(counters)
    }

    fn check_state_path(&self, config: &Config) -> Result<()> {
        let parent = config.state_path.parent().unwrap_or_else(|| Path::new("."));
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

    fn now(&self, config: &Config) -> DateTime<FixedOffset> {
        Utc::now().with_timezone(config.cycle_anchor.offset())
    }

    fn config(&self) -> Result<Config> {
        self.config
            .read()
            .map_err(|_| anyhow::anyhow!("config lock poisoned"))
            .map(|guard| guard.clone())
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
    mode.as_str()
}

fn apply_used_calibration(config: &Config, state: &mut State, target_used: u64) {
    let current_rx = apply_offset(state.cycle_rx_bytes, state.calibration_rx_offset);
    let current_tx = apply_offset(state.cycle_tx_bytes, state.calibration_tx_offset);
    let (target_rx, target_tx) = match config.billing_mode {
        BillingMode::Rx => (target_used, current_tx),
        BillingMode::Tx => (current_rx, target_used),
        BillingMode::Total => split_total_target(target_used, current_rx, current_tx),
        BillingMode::Max => align_max_target(target_used, current_rx, current_tx),
    };

    state.calibration_rx_offset = i128::from(target_rx) - i128::from(state.cycle_rx_bytes);
    state.calibration_tx_offset = i128::from(target_tx) - i128::from(state.cycle_tx_bytes);
}

fn split_total_target(target_used: u64, current_rx: u64, current_tx: u64) -> (u64, u64) {
    let current_total = u128::from(current_rx) + u128::from(current_tx);
    if current_total == 0 {
        return (target_used, 0);
    }

    let target_rx = (u128::from(target_used) * u128::from(current_rx) / current_total)
        .min(u128::from(u64::MAX)) as u64;
    (target_rx, target_used.saturating_sub(target_rx))
}

fn align_max_target(target_used: u64, current_rx: u64, current_tx: u64) -> (u64, u64) {
    if current_rx >= current_tx {
        (target_used, current_tx.min(target_used))
    } else {
        (current_rx.min(target_used), target_used)
    }
}

fn config_snapshot(config: &Config) -> ConfigSnapshot {
    ConfigSnapshot {
        listen_addr: config.listen_addr.to_string(),
        interfaces: config.interfaces.clone(),
        node_id: config.node_id.clone(),
        quota_bytes: config.quota_bytes,
        billing_mode: billing_mode_name(config.billing_mode),
        traffic_cycle_anchor: config.cycle_anchor,
        traffic_cycle_months: config.cycle_months,
        state_path: config.state_path.display().to_string(),
    }
}
