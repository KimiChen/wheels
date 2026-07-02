use anyhow::{Context, Result};
use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    io::Write,
    net::Ipv4Addr,
    path::{Path, PathBuf},
};

pub const FULLCHAIN_FILE: &str = "fullchain.pem";
pub const PRIVKEY_FILE: &str = "privkey.pem";
pub const CERT_FILE: &str = "cert.pem";
pub const CHAIN_FILE: &str = "chain.pem";
pub const METADATA_FILE: &str = "metadata.json";

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct CertificateMetadata {
    pub ip: String,
    pub hostname: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub certificate_path: Option<String>,
    pub not_before: DateTime<Utc>,
    pub not_after: DateTime<Utc>,
    pub issued_at: DateTime<Utc>,
    pub renewed_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_requested_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_source_ip: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_bundle_sha256: Option<String>,
}

#[derive(Clone, Debug)]
pub struct CertificateMaterial {
    pub fullchain_pem: Vec<u8>,
    pub privkey_pem: Vec<u8>,
    pub cert_pem: Vec<u8>,
    pub chain_pem: Vec<u8>,
    pub metadata: CertificateMetadata,
}

#[derive(Clone, Debug)]
pub struct StoredCertificate {
    pub directory: PathBuf,
    pub metadata: CertificateMetadata,
}

impl StoredCertificate {
    pub fn file_path(&self, name: &str) -> PathBuf {
        self.directory.join(name)
    }

    pub fn renewal_due(&self, renew_before_days: i64) -> bool {
        self.metadata.not_after <= Utc::now() + Duration::days(renew_before_days)
    }
}

#[derive(Clone, Debug)]
pub struct CertStore {
    root: PathBuf,
}

impl CertStore {
    pub fn new(root: PathBuf) -> Self {
        Self { root }
    }

    pub fn load(&self, ip: Ipv4Addr) -> Result<Option<StoredCertificate>> {
        let directory = self.certificate_dir(ip);
        let metadata_path = directory.join(METADATA_FILE);
        if !metadata_path.exists() {
            return Ok(None);
        }

        for name in [FULLCHAIN_FILE, PRIVKEY_FILE, CERT_FILE, CHAIN_FILE] {
            if !directory.join(name).exists() {
                return Ok(None);
            }
        }

        let content = fs::read_to_string(&metadata_path)
            .with_context(|| format!("failed to read {}", metadata_path.display()))?;
        let metadata = serde_json::from_str(&content)
            .with_context(|| format!("failed to parse {}", metadata_path.display()))?;
        Ok(Some(StoredCertificate {
            directory,
            metadata,
        }))
    }

    pub fn write_material(&self, ip: Ipv4Addr, material: CertificateMaterial) -> Result<()> {
        let directory = self.certificate_dir(ip);
        fs::create_dir_all(&directory)
            .with_context(|| format!("failed to create {}", directory.display()))?;
        restrict_dir(&directory)?;

        write_secure_file(
            &directory.join(FULLCHAIN_FILE),
            &material.fullchain_pem,
            0o644,
        )?;
        write_secure_file(&directory.join(PRIVKEY_FILE), &material.privkey_pem, 0o600)?;
        write_secure_file(&directory.join(CERT_FILE), &material.cert_pem, 0o644)?;
        write_secure_file(&directory.join(CHAIN_FILE), &material.chain_pem, 0o644)?;

        let mut metadata = material.metadata;
        metadata.certificate_path = Some(directory.display().to_string());
        self.write_metadata(&directory, &metadata)
    }

    pub fn update_request_metadata(
        &self,
        ip: Ipv4Addr,
        source_ip: &str,
        bundle_sha256: &str,
    ) -> Result<CertificateMetadata> {
        let stored = self
            .load(ip)?
            .with_context(|| format!("certificate metadata for {ip} disappeared"))?;
        let mut metadata = stored.metadata;
        metadata.last_requested_at = Some(Utc::now());
        metadata.last_source_ip = Some(source_ip.to_string());
        metadata.last_bundle_sha256 = Some(bundle_sha256.to_string());
        self.write_metadata(&stored.directory, &metadata)?;
        Ok(metadata)
    }

    fn certificate_dir(&self, ip: Ipv4Addr) -> PathBuf {
        self.root.join(ip.to_string())
    }

    fn write_metadata(&self, directory: &Path, metadata: &CertificateMetadata) -> Result<()> {
        let content = serde_json::to_vec_pretty(metadata)?;
        write_secure_file(&directory.join(METADATA_FILE), &content, 0o600)
    }
}

fn write_secure_file(path: &Path, bytes: &[u8], mode: u32) -> Result<()> {
    let tmp_path = path.with_extension("tmp");
    {
        let mut file = fs::File::create(&tmp_path)
            .with_context(|| format!("failed to create {}", tmp_path.display()))?;
        file.write_all(bytes)
            .with_context(|| format!("failed to write {}", tmp_path.display()))?;
        file.sync_all()
            .with_context(|| format!("failed to sync {}", tmp_path.display()))?;
    }
    set_permissions(&tmp_path, mode)?;
    replace_file(&tmp_path, path)
}

fn replace_file(from: &Path, to: &Path) -> Result<()> {
    if cfg!(windows) && to.exists() {
        fs::remove_file(to).with_context(|| format!("failed to replace {}", to.display()))?;
    }
    fs::rename(from, to)
        .with_context(|| format!("failed to install {} from {}", to.display(), from.display()))
}

fn restrict_dir(path: &Path) -> Result<()> {
    set_permissions(path, 0o700)
}

#[cfg(unix)]
fn set_permissions(path: &Path, mode: u32) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(mode))
        .with_context(|| format!("failed to set permissions on {}", path.display()))
}

#[cfg(not(unix))]
fn set_permissions(_path: &Path, _mode: u32) -> Result<()> {
    Ok(())
}
