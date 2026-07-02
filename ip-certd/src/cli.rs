use clap::{Parser, Subcommand};
use std::path::PathBuf;

#[derive(Debug, Parser)]
#[command(author, version, about)]
pub struct Cli {
    #[arg(long, default_value = "/etc/ip-certd/config.toml")]
    pub config: PathBuf,

    #[arg(long, default_value = "/etc/ip-certd/iplist.toml")]
    pub iplist: PathBuf,

    #[command(subcommand)]
    pub command: Option<Command>,
}

#[derive(Clone, Debug, Subcommand)]
pub enum Command {
    Serve,
    Check,
}
