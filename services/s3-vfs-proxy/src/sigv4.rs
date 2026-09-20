//! Minimal AWS SigV4 request signing for talking to the upstream Ceph RGW.
//! We only ever sign requests we construct ourselves to go out to RGW -
//! we deliberately don't verify the *inbound* signature from Filestash,
//! since this service is only reachable inside the cluster network (see
//! README note in main.rs).

use chrono::Utc;
use hmac::{Hmac, Mac};
use sha2::{Digest, Sha256};

type HmacSha256 = Hmac<Sha256>;

pub const UNSIGNED_PAYLOAD: &str = "UNSIGNED-PAYLOAD";
const EMPTY_PAYLOAD_HASH: &str =
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

pub struct SignedRequest {
    pub amz_date: String,
    pub content_sha256: String,
    pub authorization: String,
}

fn sha256_hex(data: &[u8]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(data);
    hex::encode(hasher.finalize())
}

fn hmac(key: &[u8], data: &[u8]) -> Vec<u8> {
    let mut mac = HmacSha256::new_from_slice(key).expect("HMAC accepts any key length");
    mac.update(data);
    mac.finalize().into_bytes().to_vec()
}

/// AWS-style percent-encoding: everything except unreserved characters
/// (A-Za-z0-9-_.~) is percent-encoded. `/` is left alone when encoding a
/// path (segments are joined by literal slashes) but escaped elsewhere
/// (e.g. query string keys/values).
pub fn uri_encode(input: &str, encode_slash: bool) -> String {
    let mut out = String::with_capacity(input.len());
    for byte in input.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(byte as char)
            }
            b'/' if !encode_slash => out.push('/'),
            _ => out.push_str(&format!("%{:02X}", byte)),
        }
    }
    out
}

fn canonical_uri(path: &str) -> String {
    if path.is_empty() {
        return "/".to_string();
    }
    uri_encode(path, false)
}

fn canonical_query(query_pairs: &[(String, String)]) -> String {
    let mut pairs: Vec<(String, String)> = query_pairs
        .iter()
        .map(|(k, v)| (uri_encode(k, true), uri_encode(v, true)))
        .collect();
    pairs.sort();
    pairs
        .into_iter()
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join("&")
}

/// Sign a request bound for `host` (e.g. "rook-ceph-rgw-ceph-objectstore.rook-ceph.svc.cluster.local")
/// and return the headers that need to be attached to it: `x-amz-date`,
/// `x-amz-content-sha256` and `Authorization`.
///
/// `body_hash` should be `Some(sha256_hex(body))` when the full body is
/// already in memory (small XML payloads), or `None` to fall back to
/// `UNSIGNED-PAYLOAD` for requests we stream through without buffering
/// (PutObject / UploadPart bodies).
#[allow(clippy::too_many_arguments)]
pub fn sign(
    method: &str,
    host: &str,
    path: &str,
    query_pairs: &[(String, String)],
    extra_signed_headers: &[(&str, &str)],
    body_hash: Option<&str>,
    access_key: &str,
    secret_key: &str,
    region: &str,
) -> SignedRequest {
    let now = Utc::now();
    let amz_date = now.format("%Y%m%dT%H%M%SZ").to_string();
    let date_stamp = now.format("%Y%m%d").to_string();

    let content_sha256 = match body_hash {
        Some(h) => h.to_string(),
        None => UNSIGNED_PAYLOAD.to_string(),
    };
    let content_sha256 = if content_sha256.is_empty() {
        EMPTY_PAYLOAD_HASH.to_string()
    } else {
        content_sha256
    };

    // Signed header set: host, x-amz-date, x-amz-content-sha256, plus
    // whatever the caller needs signed (e.g. x-amz-copy-source).
    let mut headers: Vec<(String, String)> = vec![
        ("host".to_string(), host.to_string()),
        ("x-amz-content-sha256".to_string(), content_sha256.clone()),
        ("x-amz-date".to_string(), amz_date.clone()),
    ];
    for (k, v) in extra_signed_headers {
        headers.push((k.to_lowercase(), v.trim().to_string()));
    }
    headers.sort_by(|a, b| a.0.cmp(&b.0));

    let canonical_headers: String = headers
        .iter()
        .map(|(k, v)| format!("{k}:{v}\n"))
        .collect::<Vec<_>>()
        .join("");
    let signed_headers: String = headers
        .iter()
        .map(|(k, _)| k.as_str())
        .collect::<Vec<_>>()
        .join(";");

    let canonical_request = format!(
        "{method}\n{uri}\n{query}\n{headers}\n{signed}\n{payload}",
        method = method,
        uri = canonical_uri(path),
        query = canonical_query(query_pairs),
        headers = canonical_headers,
        signed = signed_headers,
        payload = content_sha256,
    );

    let credential_scope = format!("{date_stamp}/{region}/s3/aws4_request");
    let string_to_sign = format!(
        "AWS4-HMAC-SHA256\n{amz_date}\n{scope}\n{hash}",
        amz_date = amz_date,
        scope = credential_scope,
        hash = sha256_hex(canonical_request.as_bytes()),
    );

    let k_date = hmac(format!("AWS4{secret_key}").as_bytes(), date_stamp.as_bytes());
    let k_region = hmac(&k_date, region.as_bytes());
    let k_service = hmac(&k_region, b"s3");
    let k_signing = hmac(&k_service, b"aws4_request");
    let signature = hex::encode(hmac(&k_signing, string_to_sign.as_bytes()));

    let authorization = format!(
        "AWS4-HMAC-SHA256 Credential={access_key}/{scope}, SignedHeaders={signed_headers}, Signature={signature}",
        access_key = access_key,
        scope = credential_scope,
    );

    SignedRequest {
        amz_date,
        content_sha256,
        authorization,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sign_is_deterministic_for_same_inputs_modulo_timestamp() {
        // Not asserting an exact signature (that would pin us to a fixed
        // clock); just checking the function runs end to end and produces
        // the expected Authorization header shape for a known access key.
        let signed = sign(
            "GET",
            "rook-ceph-rgw-ceph-objectstore.rook-ceph.svc.cluster.local",
            "/media-library/foo.txt",
            &[],
            &[],
            Some("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"),
            "AKIDEXAMPLE",
            "secret",
            "us-east-1",
        );
        assert!(signed.authorization.starts_with("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"));
        assert!(signed.authorization.contains("SignedHeaders=host;x-amz-content-sha256;x-amz-date"));
        assert_eq!(signed.amz_date.len(), "20240101T000000Z".len());
    }

    #[test]
    fn uri_encode_leaves_slash_alone_for_paths() {
        assert_eq!(uri_encode("/a b/c.txt", false), "/a%20b/c.txt");
    }

    #[test]
    fn uri_encode_escapes_slash_for_query() {
        assert_eq!(uri_encode("a/b", true), "a%2Fb");
    }
}
