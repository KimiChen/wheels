use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(version, about)]
pub struct Cli {
    #[arg(long, global = true, default_value = "/etc/vps-trafficd/config.toml")]
    pub config: String,

    #[command(subcommand)]
    pub command: Option<Command>,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Check configuration, network counters and state file permissions.
    Check,
    /// Align current billing-cycle counters with a provider panel.
    Calibrate {
        #[arg(long)]
        rx: u64,
        #[arg(long)]
        tx: u64,
    },
}
