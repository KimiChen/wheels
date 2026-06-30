use clap::{Parser, Subcommand};

#[derive(Debug, Parser)]
#[command(version, about)]
pub struct Cli {
    #[arg(long, global = true, default_value = "/etc/resend-ticketd/.env")]
    pub config: String,

    #[command(subcommand)]
    pub command: Option<Command>,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Start the HTTPS ticket service.
    Serve,
    /// Validate configuration and initialize database schema.
    Check,
    /// Issue or renew the configured TLS certificate through lego.
    Cert {
        #[command(subcommand)]
        command: CertCommand,
    },
}

#[derive(Debug, Subcommand)]
pub enum CertCommand {
    /// Request a new certificate.
    Issue,
    /// Renew the existing certificate and restart the service on success.
    Renew,
}
