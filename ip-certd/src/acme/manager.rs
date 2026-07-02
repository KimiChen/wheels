use crate::{
    acme::{account::AcmeAccountConfig, dns01::Dns01Challenge},
    cert_store::CertificateMaterial,
    cloudflare::DnsProvider,
    config::AcmeConfig,
};
use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use instant_acme::{
    AuthorizationStatus, ChallengeType, Identifier, NewOrder, OrderStatus, RetryPolicy,
};
use std::{net::Ipv4Addr, sync::Arc, time::Duration};
use x509_parser::{parse_x509_certificate, pem::parse_x509_pem};

#[derive(Clone)]
pub struct AcmeManager {
    config: AcmeConfig,
    dns: Arc<dyn DnsProvider>,
}

impl AcmeManager {
    pub fn new(config: AcmeConfig, dns: Arc<dyn DnsProvider>) -> Self {
        Self { config, dns }
    }

    pub fn enabled(&self) -> bool {
        self.config.enabled
    }

    pub async fn issue_or_renew(
        &self,
        ip: Ipv4Addr,
        hostname: &str,
        source_ip: &str,
    ) -> Result<CertificateMaterial> {
        let mut cleanup = Vec::new();
        let result = self
            .issue_or_renew_inner(ip, hostname, source_ip, &mut cleanup)
            .await;

        for (name, value) in cleanup {
            if let Err(error) = self.dns.delete_txt(&name, &value).await {
                tracing::warn!(%error, "failed to clean up ACME DNS-01 TXT record");
            }
        }

        result
    }

    async fn issue_or_renew_inner(
        &self,
        ip: Ipv4Addr,
        hostname: &str,
        source_ip: &str,
        cleanup: &mut Vec<(String, String)>,
    ) -> Result<CertificateMaterial> {
        if !self.config.enabled {
            bail!("ACME is disabled");
        }

        let account_config = AcmeAccountConfig::from_config(&self.config);
        let account = account_config.load_or_create_account().await?;
        let identifiers = [Identifier::Dns(hostname.to_string())];
        let mut order = account
            .new_order(&NewOrder::new(&identifiers))
            .await
            .context("failed to create ACME order")?;

        let challenge_helper = Dns01Challenge::new(
            self.dns.clone(),
            60,
            Duration::from_secs(self.config.dns_propagation_timeout_seconds),
        );

        let mut authorizations = order.authorizations();
        while let Some(result) = authorizations.next().await {
            let mut authz = result.context("failed to fetch ACME authorization")?;
            match authz.status {
                AuthorizationStatus::Valid => continue,
                AuthorizationStatus::Pending => {}
                status => bail!("unexpected ACME authorization status: {status:?}"),
            }

            let mut challenge = authz
                .challenge(ChallengeType::Dns01)
                .ok_or_else(|| anyhow::anyhow!("ACME authorization has no DNS-01 challenge"))?;
            let record_name = format!("_acme-challenge.{}", challenge.identifier());
            let record_value = challenge.key_authorization().dns_value();
            challenge_helper
                .present(&record_name, &record_value)
                .await
                .with_context(|| format!("failed to present DNS-01 challenge for {hostname}"))?;
            cleanup.push((record_name.clone(), record_value.clone()));
            challenge_helper
                .wait_for_propagation(&record_name, &record_value)
                .await?;
            challenge
                .set_ready()
                .await
                .context("failed to mark ACME DNS-01 challenge ready")?;
        }
        drop(authorizations);

        let retry_policy = RetryPolicy::default()
            .initial_delay(Duration::from_secs(2))
            .timeout(Duration::from_secs(
                self.config.dns_propagation_timeout_seconds + 120,
            ));
        let status = order
            .poll_ready(&retry_policy)
            .await
            .context("failed while waiting for ACME order readiness")?;
        if status != OrderStatus::Ready {
            bail!("unexpected ACME order status before finalization: {status:?}");
        }

        let private_key_pem = order
            .finalize()
            .await
            .context("failed to finalize ACME order")?;
        let fullchain_pem = order
            .poll_certificate(&retry_policy)
            .await
            .context("failed to download ACME certificate chain")?;
        let (cert_pem, chain_pem) = split_certificate_chain(&fullchain_pem)?;
        let (not_before, not_after) = leaf_validity(&cert_pem)?;
        let now = Utc::now();

        Ok(CertificateMaterial {
            fullchain_pem: fullchain_pem.into_bytes(),
            privkey_pem: private_key_pem.into_bytes(),
            cert_pem: cert_pem.into_bytes(),
            chain_pem: chain_pem.into_bytes(),
            metadata: crate::cert_store::CertificateMetadata {
                ip: ip.to_string(),
                hostname: hostname.to_string(),
                certificate_path: None,
                not_before,
                not_after,
                issued_at: now,
                renewed_at: now,
                last_requested_at: Some(now),
                last_source_ip: Some(source_ip.to_string()),
                last_bundle_sha256: None,
            },
        })
    }
}

fn split_certificate_chain(fullchain_pem: &str) -> Result<(String, String)> {
    let mut certificates = Vec::new();
    let mut remaining = fullchain_pem;
    const BEGIN: &str = "-----BEGIN CERTIFICATE-----";
    const END: &str = "-----END CERTIFICATE-----";

    while let Some(begin) = remaining.find(BEGIN) {
        let after_begin = &remaining[begin..];
        let Some(end) = after_begin.find(END) else {
            bail!("certificate chain contains an unterminated certificate PEM block");
        };
        let block_end = end + END.len();
        let mut block = after_begin[..block_end].to_string();
        block.push('\n');
        certificates.push(block);
        remaining = &after_begin[block_end..];
    }

    if certificates.is_empty() {
        bail!("ACME response did not contain any certificate PEM blocks");
    }

    let cert_pem = certificates.remove(0);
    let chain_pem = certificates.join("");
    Ok((cert_pem, chain_pem))
}

fn leaf_validity(cert_pem: &str) -> Result<(DateTime<Utc>, DateTime<Utc>)> {
    let (_, pem) =
        parse_x509_pem(cert_pem.as_bytes()).map_err(|error| anyhow::anyhow!("{error}"))?;
    let (_, cert) =
        parse_x509_certificate(&pem.contents).map_err(|error| anyhow::anyhow!("{error}"))?;
    let validity = cert.validity();
    let not_before = DateTime::<Utc>::from_timestamp(validity.not_before.timestamp(), 0)
        .context("certificate not_before is outside supported timestamp range")?;
    let not_after = DateTime::<Utc>::from_timestamp(validity.not_after.timestamp(), 0)
        .context("certificate not_after is outside supported timestamp range")?;
    Ok((not_before, not_after))
}
