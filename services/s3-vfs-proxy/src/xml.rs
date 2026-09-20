//! Small, targeted XML helpers. We never need a full S3 XML model: only a
//! handful of response shapes ever mention a bucket name or object key, so
//! we either build tiny fixed documents by hand (ListBuckets, the
//! synthesized top-level folder listing) or stream-rewrite the upstream
//! RGW response, prefixing/replacing just the tags that matter.

use quick_xml::events::{BytesText, Event};
use quick_xml::{Reader, Writer};

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

pub fn list_buckets(virtual_bucket: &str) -> String {
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Owner><ID>vfs</ID><DisplayName>vfs</DisplayName></Owner>
  <Buckets>
    <Bucket><Name>{name}</Name><CreationDate>1970-01-01T00:00:00.000Z</CreationDate></Bucket>
  </Buckets>
</ListAllMyBucketsResult>"#,
        name = escape(virtual_bucket)
    )
}

/// The virtual bucket's root listing: one CommonPrefix per configured real
/// bucket whose name starts with `prefix`, synthesized locally with no
/// upstream call at all (there's nothing to list yet - we haven't
/// descended into a real bucket).
pub fn top_level_listing(virtual_bucket: &str, prefix: &str, folders: &[&str]) -> String {
    let common_prefixes: String = folders
        .iter()
        .filter(|f| f.starts_with(prefix))
        .map(|f| format!("  <CommonPrefixes><Prefix>{}/</Prefix></CommonPrefixes>\n", escape(f)))
        .collect();
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>{name}</Name>
  <Prefix>{prefix}</Prefix>
  <Delimiter>/</Delimiter>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
{common_prefixes}</ListBucketResult>"#,
        name = escape(virtual_bucket),
        prefix = escape(prefix),
        common_prefixes = common_prefixes,
    )
}

enum TagTransform<'a> {
    Prefix(&'a str),
    Replace(&'a str),
}

fn transform_tag_text(body: &[u8], transforms: &[(&str, TagTransform)]) -> Vec<u8> {
    let mut reader = Reader::from_reader(body);
    reader.config_mut().trim_text(false);
    let mut writer = Writer::new(Vec::new());
    let mut tag_stack: Vec<String> = Vec::new();

    loop {
        match reader.read_event() {
            Ok(Event::Eof) => break,
            Ok(Event::Start(e)) => {
                let name = String::from_utf8_lossy(e.local_name().as_ref()).to_string();
                tag_stack.push(name);
                let _ = writer.write_event(Event::Start(e));
            }
            Ok(Event::End(e)) => {
                tag_stack.pop();
                let _ = writer.write_event(Event::End(e));
            }
            Ok(Event::Text(t)) => {
                let current = tag_stack.last().map(|s| s.as_str()).unwrap_or("");
                let matched = transforms.iter().find(|(tag, _)| *tag == current);
                match matched {
                    Some((_, TagTransform::Prefix(p))) => {
                        let text = t.unescape().unwrap_or_default();
                        let new_text = format!("{p}{text}");
                        let _ = writer.write_event(Event::Text(BytesText::new(&new_text)));
                    }
                    Some((_, TagTransform::Replace(v))) => {
                        let _ = writer.write_event(Event::Text(BytesText::new(v)));
                    }
                    None => {
                        let _ = writer.write_event(Event::Text(t));
                    }
                }
            }
            Ok(other) => {
                let _ = writer.write_event(other);
            }
            Err(_) => break,
        }
    }
    writer.into_inner()
}

/// Rewrite an upstream ListObjectsV2 response so it looks like it came
/// from `virtual_bucket`, with every Key/Prefix re-prefixed by the real
/// bucket's folder name (e.g. "media-library/") that we stripped before
/// forwarding the request upstream.
pub fn rewrite_list_objects(body: &[u8], virtual_bucket: &str, folder_prefix: &str) -> Vec<u8> {
    transform_tag_text(
        body,
        &[
            ("Name", TagTransform::Replace(virtual_bucket)),
            ("Key", TagTransform::Prefix(folder_prefix)),
            ("Prefix", TagTransform::Prefix(folder_prefix)),
        ],
    )
}

/// Rewrite CreateMultipartUpload / CompleteMultipartUpload responses,
/// which echo Bucket/Key (and Location for Complete).
pub fn rewrite_multipart_response(
    body: &[u8],
    virtual_bucket: &str,
    folder_prefix: &str,
) -> Vec<u8> {
    transform_tag_text(
        body,
        &[
            ("Bucket", TagTransform::Replace(virtual_bucket)),
            ("Key", TagTransform::Prefix(folder_prefix)),
        ],
    )
}

/// Parse a DeleteObjects request body (`<Delete><Object><Key>...</Key></Object>...</Delete>`)
/// into the plain list of requested (virtual) keys, in order.
pub fn parse_delete_keys(body: &[u8]) -> Vec<String> {
    let mut reader = Reader::from_reader(body);
    reader.config_mut().trim_text(true);
    let mut keys = Vec::new();
    let mut in_key = false;
    loop {
        match reader.read_event() {
            Ok(Event::Eof) => break,
            Ok(Event::Start(e)) if e.local_name().as_ref() == b"Key" => in_key = true,
            Ok(Event::End(e)) if e.local_name().as_ref() == b"Key" => in_key = false,
            Ok(Event::Text(t)) if in_key => {
                if let Ok(text) = t.unescape() {
                    keys.push(text.to_string());
                }
            }
            Ok(_) => {}
            Err(_) => break,
        }
    }
    keys
}

/// Build a DeleteObjects request body for the (already bucket-relative)
/// keys destined for one real bucket.
pub fn build_delete_request(keys: &[&str]) -> String {
    let objects: String = keys
        .iter()
        .map(|k| format!("<Object><Key>{}</Key></Object>", escape(k)))
        .collect();
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?><Delete>{objects}</Delete>"#,
        objects = objects
    )
}

/// Merge a real bucket's DeleteObjects response into the aggregate result,
/// re-prefixing every Deleted/Error Key with `folder_prefix` so the final
/// response's keys match what the client asked to delete.
pub fn rewrite_delete_response(body: &[u8], folder_prefix: &str) -> Vec<u8> {
    transform_tag_text(body, &[("Key", TagTransform::Prefix(folder_prefix))])
}

/// Wrap several already-rewritten `<Deleted>.../<Error>...` fragments
/// (with their outer `<DeleteResult>` stripped) into one final response.
pub fn wrap_delete_result(fragments: &[String]) -> String {
    format!(
        r#"<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">{}</DeleteResult>"#,
        fragments.join("")
    )
}

/// Extract the inner content of a `<DeleteResult>...</DeleteResult>` document
/// (i.e. everything between the outer tags), for folding into `wrap_delete_result`.
pub fn inner_of_delete_result(body: &[u8]) -> String {
    let text = String::from_utf8_lossy(body);
    let Some(tag_start) = text.find("<DeleteResult") else {
        return String::new();
    };
    let Some(tag_end_offset) = text[tag_start..].find('>') else {
        return String::new();
    };
    let open_end = tag_start + tag_end_offset + 1;
    let Some(close_start) = text.rfind("</DeleteResult>") else {
        return String::new();
    };
    if open_end <= close_start {
        text[open_end..close_start].to_string()
    } else {
        String::new()
    }
}

fn build_error_fragment(key: &str, code: &str, message: &str) -> String {
    format!(
        "<Error><Key>{}</Key><Code>{}</Code><Message>{}</Message></Error>",
        escape(key),
        escape(code),
        escape(message)
    )
}

pub fn error_fragment(key: &str, code: &str, message: &str) -> String {
    build_error_fragment(key, code, message)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rewrite_list_objects_prefixes_keys_and_common_prefixes() {
        let upstream = br#"<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>media-library</Name>
  <Prefix>photos/</Prefix>
  <Contents><Key>photos/a.jpg</Key><Size>10</Size></Contents>
  <CommonPrefixes><Prefix>photos/sub/</Prefix></CommonPrefixes>
</ListBucketResult>"#;
        let out = rewrite_list_objects(upstream, "vfs", "media-library/");
        let out = String::from_utf8(out).unwrap();
        assert!(out.contains("<Name>vfs</Name>"));
        assert!(out.contains("<Key>media-library/photos/a.jpg</Key>"));
        assert!(out.contains("<Prefix>media-library/photos/sub/</Prefix>"));
    }

    #[test]
    fn top_level_listing_lists_configured_folders() {
        let xml = top_level_listing("vfs", "", &["media-library", "immich-external-library"]);
        assert!(xml.contains("<CommonPrefixes><Prefix>media-library/</Prefix></CommonPrefixes>"));
        assert!(xml.contains("<CommonPrefixes><Prefix>immich-external-library/</Prefix></CommonPrefixes>"));
    }

    #[test]
    fn top_level_listing_filters_by_prefix() {
        let xml = top_level_listing("vfs", "media", &["media-library", "immich-external-library"]);
        assert!(xml.contains("media-library/"));
        assert!(!xml.contains("immich-external-library/"));
    }

    #[test]
    fn parse_delete_keys_extracts_in_order() {
        let body = br#"<Delete><Object><Key>media-library/a.txt</Key></Object><Object><Key>immich-external-library/b.txt</Key></Object></Delete>"#;
        let keys = parse_delete_keys(body);
        assert_eq!(keys, vec!["media-library/a.txt", "immich-external-library/b.txt"]);
    }

    #[test]
    fn build_delete_request_escapes_keys() {
        let body = build_delete_request(&["a&b.txt"]);
        assert!(body.contains("<Key>a&amp;b.txt</Key>"));
    }

    #[test]
    fn inner_of_delete_result_strips_outer_tags() {
        let body = br#"<?xml version="1.0"?><DeleteResult xmlns="foo"><Deleted><Key>a</Key></Deleted></DeleteResult>"#;
        assert_eq!(inner_of_delete_result(body), "<Deleted><Key>a</Key></Deleted>");
    }

    #[test]
    fn rewrite_multipart_response_rewrites_bucket_and_key() {
        let body = br#"<InitiateMultipartUploadResult><Bucket>media-library</Bucket><Key>a/b.mp4</Key><UploadId>xyz</UploadId></InitiateMultipartUploadResult>"#;
        let out = rewrite_multipart_response(body, "vfs", "media-library/");
        let out = String::from_utf8(out).unwrap();
        assert!(out.contains("<Bucket>vfs</Bucket>"));
        assert!(out.contains("<Key>media-library/a/b.mp4</Key>"));
        assert!(out.contains("<UploadId>xyz</UploadId>"));
    }
}
