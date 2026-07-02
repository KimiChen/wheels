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
    pub usage_ratio: f64,
    pub updated_at: DateTime<FixedOffset>,
}

#[derive(Debug, Serialize)]
pub struct ConfigSnapshot {
    pub listen_addr: String,
    pub interfaces: Vec<String>,
    pub node_id: String,
    pub quota_bytes: u64,
    pub billing_mode: &'static str,
    pub billing_cycle_anchor: DateTime<FixedOffset>,
    pub billing_cycle_months: u32,
    pub traffic_cycle_anchor: DateTime<FixedOffset>,
    pub traffic_cycle_months: u32,
    pub state_path: String,
}

#[derive(Debug, Deserialize)]
pub struct ConfigUpdate {
    pub billing_cycle_anchor: DateTime<FixedOffset>,
    pub billing_cycle_months: u32,
    pub traffic_cycle_anchor: DateTime<FixedOffset>,
    pub traffic_cycle_months: u32,
    pub quota_bytes: u64,
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
        };
        let remaining_bytes = config.quota_bytes.saturating_sub(used_bytes);
        let usage_ratio = used_bytes as f64 / config.quota_bytes as f64;

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
            usage_ratio,
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
        if update.billing_cycle_months == 0 {
            bail!("billing_cycle_months must be greater than zero");
        }
        if update.traffic_cycle_months == 0 {
            bail!("traffic_cycle_months must be greater than zero");
        }
        if update.quota_bytes == 0 {
            bail!("quota_bytes must be greater than zero");
        }

        let mut next = self.config()?;
        next.billing_cycle_anchor = Some(update.billing_cycle_anchor);
        next.billing_cycle_months = Some(update.billing_cycle_months);
        next.cycle_anchor = update.traffic_cycle_anchor;
        next.cycle_months = update.traffic_cycle_months;
        next.quota_bytes = update.quota_bytes;
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
        self.update_state(&next)?;
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

fn config_snapshot(config: &Config) -> ConfigSnapshot {
    ConfigSnapshot {
        listen_addr: config.listen_addr.to_string(),
        interfaces: config.interfaces.clone(),
        node_id: config.node_id.clone(),
        quota_bytes: config.quota_bytes,
        billing_mode: billing_mode_name(config.billing_mode),
        billing_cycle_anchor: config.billing_cycle_anchor.unwrap_or(config.cycle_anchor),
        billing_cycle_months: config.billing_cycle_months.unwrap_or(config.cycle_months),
        traffic_cycle_anchor: config.cycle_anchor,
        traffic_cycle_months: config.cycle_months,
        state_path: config.state_path.display().to_string(),
    }
}
