use crate::config::AcmeConfig;

#[derive(Clone, Debug)]
pub struct AcmeAccountConfig {
    pub email: String,
    pub directory_url: String,
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
        }
    }
}
