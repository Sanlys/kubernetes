use serde::Deserialize;
use std::error::Error;
use std::fs;

fn default_region() -> String {
    "us-east-1".to_string()
}

/// Static configuration: which real buckets are exposed as top-level
/// "folders" inside the single virtual bucket, and where to reach the
/// real Ceph RGW endpoint. Bucket names double as both the virtual
/// folder name and the real upstream bucket name.
#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub virtual_bucket: String,
    pub upstream_endpoint: String,
    #[serde(default = "default_region")]
    pub region: String,
    pub buckets: Vec<String>,
}

impl Config {
    pub fn load(path: &str) -> Result<Config, Box<dyn Error>> {
        let raw = fs::read_to_string(path)?;
        let cfg: Config = serde_json::from_str(&raw)?;
        if cfg.buckets.is_empty() {
            return Err("config: `buckets` must list at least one bucket".into());
        }
        Ok(cfg)
    }

    /// Real bucket name for a given top-level path segment, if configured.
    pub fn resolve_bucket(&self, name: &str) -> Option<&str> {
        self.buckets
            .iter()
            .find(|b| b.as_str() == name)
            .map(|b| b.as_str())
    }
}
