use crate::config::AcmeConfig;
use anyhow::{Context, Result};
use instant_acme::{Account, AccountCredentials, NewAccount};
use std::{
    fs,
    io::Write,
    path::{Path, PathBuf},
};

#[derive(Clone, Debug)]
pub struct AcmeAccountConfig {
    pub email: String,
    pub directory_url: String,
    pub credentials_path: PathBuf,
}

impl AcmeAccountConfig {
    pub fn from_config(config: &AcmeConfig) -> Self {
        let directory_url = if config.use_staging {
            config.staging_directory.clone()
        } else {
            config.directory.clone()
        };
        Self {
            email: config.email.clone(),
            directory_url,
            credentials_path: config.storage.join("accounts").join(if config.use_staging {
                "staging-account.json"
            } else {
                "production-account.json"
            }),
        }
    }

    pub async fn load_or_create_account(&self) -> Result<Account> {
        if self.credentials_path.exists() {
            let content = fs::read_to_string(&self.credentials_path).with_context(|| {
                format!(
                    "failed to read ACME account credentials from {}",
                    self.credentials_path.display()
                )
            })?;
            let credentials: AccountCredentials = serde_json::from_str(&content)
                .context("failed to parse ACME account credentials")?;
            return Account::builder()?
                .from_credentials(credentials)
                .await
                .context("failed to restore ACME account");
        }

        let contact = format!("mailto:{}", self.email.trim());
        let contacts = [contact.as_str()];
        let (account, credentials) = Account::builder()?
            .create(
                &NewAccount {
                    contact: &contacts,
                    terms_of_service_agreed: true,
                    only_return_existing: false,
                },
                self.directory_url.clone(),
                None,
            )
            .await
            .context("failed to create ACME account")?;

        write_credentials(&self.credentials_path, &credentials)?;
        Ok(account)
    }
}

fn write_credentials(path: &Path, credentials: &AccountCredentials) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("failed to create {}", parent.display()))?;
        set_permissions(parent, 0o700)?;
    }

    let content =
        serde_json::to_vec_pretty(credentials).context("failed to serialize ACME credentials")?;
    let tmp_path = path.with_extension("json.tmp");
    {
        let mut file = fs::File::create(&tmp_path)
            .with_context(|| format!("failed to create {}", tmp_path.display()))?;
        file.write_all(&content)
            .with_context(|| format!("failed to write {}", tmp_path.display()))?;
        file.sync_all()
            .with_context(|| format!("failed to sync {}", tmp_path.display()))?;
    }
    set_permissions(&tmp_path, 0o600)?;
    if cfg!(windows) && path.exists() {
        fs::remove_file(path).with_context(|| format!("failed to replace {}", path.display()))?;
    }
    fs::rename(&tmp_path, path).with_context(|| format!("failed to install {}", path.display()))?;
    Ok(())
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
