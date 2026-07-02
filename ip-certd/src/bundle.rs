use crate::cert_store::{
    StoredCertificate, CERT_FILE, CHAIN_FILE, FULLCHAIN_FILE, METADATA_FILE, PRIVKEY_FILE,
};
use anyhow::{Context, Result};
use flate2::{write::GzEncoder, Compression};
use sha2::{Digest, Sha256};
use std::{fs, io::Cursor};
use tar::{Builder, Header};

#[derive(Debug)]
pub struct CertificateBundle {
    pub archive: Vec<u8>,
    pub sha256: String,
}

pub fn create_bundle(stored: &StoredCertificate) -> Result<CertificateBundle> {
    let encoder = GzEncoder::new(Vec::new(), Compression::default());
    let mut builder = Builder::new(encoder);

    append_file(
        &mut builder,
        FULLCHAIN_FILE,
        &fs::read(stored.file_path(FULLCHAIN_FILE))?,
        0o644,
    )?;
    append_file(
        &mut builder,
        PRIVKEY_FILE,
        &fs::read(stored.file_path(PRIVKEY_FILE))?,
        0o600,
    )?;
    append_file(
        &mut builder,
        CERT_FILE,
        &fs::read(stored.file_path(CERT_FILE))?,
        0o644,
    )?;
    append_file(
        &mut builder,
        CHAIN_FILE,
        &fs::read(stored.file_path(CHAIN_FILE))?,
        0o644,
    )?;
    append_file(
        &mut builder,
        METADATA_FILE,
        &fs::read(stored.file_path(METADATA_FILE))?,
        0o600,
    )?;

    let encoder = builder
        .into_inner()
        .context("failed to finalize tar stream")?;
    let archive = encoder.finish().context("failed to finalize gzip stream")?;
    let sha256 = hex::encode(Sha256::digest(&archive));
    Ok(CertificateBundle { archive, sha256 })
}

fn append_file(
    builder: &mut Builder<GzEncoder<Vec<u8>>>,
    name: &str,
    bytes: &[u8],
    mode: u32,
) -> Result<()> {
    let mut header = Header::new_gnu();
    header.set_path(name)?;
    header.set_size(bytes.len() as u64);
    header.set_mode(mode);
    header.set_cksum();
    builder
        .append(&header, Cursor::new(bytes))
        .with_context(|| format!("failed to append {name} to certificate bundle"))
}
