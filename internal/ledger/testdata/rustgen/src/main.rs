//! Generates agentsview internal/ledger/testdata fixtures from jilog's own
//! ledger-core types (jilog 9e8e094). Run once; outputs are committed.
//!
//!   cargo run --quiet -- <out-dir>
use std::fs;
use std::path::Path;

use chrono::{DateTime, TimeZone, Utc};
use ledger_core::{Event, EventClass, PayloadTier, Segment};
use serde_json::json;
use uuid::Uuid;

fn ts(secs: i64, nanos: u32) -> DateTime<Utc> {
    Utc.timestamp_opt(secs, nanos).unwrap()
}

fn id(n: u128) -> Uuid {
    // 0190f5a2-7c3e-7000-8000-0000000000NN: a fixed v7-shaped id.
    Uuid::from_u128(0x0190f5a2_7c3e_7000_8000_000000000000u128 | n)
}

fn base(source: &str, seq: u64, n: u128, t: DateTime<Utc>) -> Event {
    Event {
        event_id: id(n),
        zone: "default".to_string(),
        source: source.to_string(),
        source_seq: seq,
        timestamp: t,
        correlation_id: None,
        causation_id: None,
        actor_ref: None,
        object_ref: None,
        event_class: EventClass::Health,
        payload_tier: PayloadTier::MetadataOnly,
        payload: None,
    }
}

fn seal_write(dir: &Path, mut seg: Segment) -> Segment {
    seg.seal().unwrap();
    seg.write_to_file(dir.join(seg.filename())).unwrap();
    seg
}

fn main() {
    let out = std::env::args().nth(1).expect("usage: gen <out-dir>");
    let dir = Path::new(&out);
    let segdir = dir.join("segments");
    fs::create_dir_all(&segdir).unwrap();
    let t0 = 1_767_323_045; // 2026-01-02T03:04:05Z

    // fixture-a-000001: serializer edge cases.
    let mut a1 = Segment::new("fixture-a", 1);
    a1.created_at = ts(t0 + 55, 0);
    a1.append(base("fixture-a", 1, 1, ts(t0, 0)));
    let mut e = base("fixture-a", 2, 2, ts(t0 + 1, 100_000_000));
    e.correlation_id = Some(id(0xc1));
    e.causation_id = Some(id(0x01));
    e.actor_ref = Some("machine:fixture-a".into());
    e.object_ref = Some("subsystem:fixture".into());
    e.event_class = EventClass::StateChange;
    e.payload_tier = PayloadTier::Structured;
    e.payload = Some(json!({
        "kind": "fixture",
        "subsystem": "fixture",
        "summary": "floats and ints",
        "floats": [1.0, 0.1, -0.0, 1.5e-7, 1e16, 1.2345678901234568e20, 1e300, 5e-324, 123456.789, 0.00001, 1234567890123456.0],
        "ints": [0, -1, 9007199254740993u64, 18446744073709551615u64, -9223372036854775808i64],
        "nested": {"z": 1, "a": {"y": null, "b": true, "c": false}},
        "empty_obj": {},
        "empty_arr": []
    }));
    a1.append(e);
    let mut e = base("fixture-a", 3, 3, ts(t0 + 2, 123_456_000));
    e.event_class = EventClass::NoteMeta;
    e.payload_tier = PayloadTier::Confidential;
    e.payload = Some(json!({
        "subsystem": "hook-unicode",
        "summary": "unicode <>& \u{2028}",
        "text": "caf\u{e9} \u{65e5}\u{672c} \u{1f600} <b>&amp;</b> \u{2028}\u{2029} \"q\" \\ \t\n\r \u{1} \u{1f} \u{7f} /"
    }));
    a1.append(e);
    let mut e = base("fixture-a", 4, 4, ts(t0 + 3, 123_456_789));
    e.event_class = EventClass::Ingest;
    e.payload = Some(json!("just a string"));
    a1.append(e);
    let mut e = base("fixture-a", 5, 5, ts(t0 + 4, 120_000_000));
    e.event_class = EventClass::Route;
    e.payload = Some(json!(42));
    a1.append(e);
    let a1 = seal_write(&segdir, a1);

    // fixture-a-000002: subsystem derivation cases.
    let mut a2 = Segment::new("fixture-a", 2);
    a2.created_at = ts(t0 + 3600, 1);
    let mut e = base("fixture-a", 1, 6, ts(t0 + 3000, 0));
    e.object_ref = Some("subsystem:from-object".into());
    e.event_class = EventClass::Decision;
    a2.append(e);
    let mut e = base("fixture-a", 2, 7, ts(t0 + 3001, 0));
    e.object_ref = Some("subsystem:ignored".into());
    e.event_class = EventClass::Claim;
    e.payload = Some(json!({"subsystem": 7, "summary": 9}));
    a2.append(e);
    let mut e = base("fixture-a", 3, 8, ts(t0 + 3002, 0));
    e.object_ref = Some("kata:proj#1".into());
    e.event_class = EventClass::Approval;
    e.payload = Some(json!(["subsystem", "x"]));
    a2.append(e);
    let a2 = seal_write(&segdir, a2);

    // host-with-dash-000007: every class x tier, AutoSi widths.
    let classes = [
        EventClass::Ingest, EventClass::Route, EventClass::Decision, EventClass::StateChange,
        EventClass::Claim, EventClass::Delivery, EventClass::Projection, EventClass::Health,
        EventClass::Approval, EventClass::NoteMeta,
    ];
    let tiers = [PayloadTier::MetadataOnly, PayloadTier::Structured, PayloadTier::Confidential];
    let nanos = [0u32, 5_000_000, 5_000, 5, 999_999_999];
    let mut b = Segment::new("host-with-dash", 7);
    b.created_at = ts(t0 + 7200, 500_000_000);
    for (i, c) in classes.iter().enumerate() {
        let mut e = base("host-with-dash", i as u64 + 1, 0x100 + i as u128, ts(t0 + 7200 + i as i64, nanos[i % nanos.len()]));
        e.zone = "ops".into();
        e.event_class = c.clone();
        e.payload_tier = tiers[i % tiers.len()].clone();
        e.actor_ref = Some("machine:host-b".into());
        e.payload = Some(json!({"i": i, "subsystem": format!("sub-{}", i % 3), "summary": format!("event {i}")}));
        b.append(e);
    }
    let b = seal_write(&segdir, b);

    // fixture-empty-000001: sealed, no events (CRC of "[]").
    let mut empty = Segment::new("fixture-empty", 1);
    empty.created_at = ts(t0, 0);
    let empty = seal_write(&segdir, empty);

    // bigseq: event source_seq above i64::MAX (import must refuse it).
    let mut big = Segment::new("fixture-bigseq", 1);
    big.created_at = ts(t0, 0);
    big.append(base("fixture-bigseq", u64::MAX, 0x200, ts(t0, 0)));
    let big = seal_write(&segdir, big);

    // fixture-lossy-000001: floats serde_json 1.0.149 (no float_roundtrip)
    // prints correctly but parses one ulp off, so jilog's own verify() of
    // this file fails while the seal CRC is right.
    let mut lossy = Segment::new("fixture-lossy", 1);
    lossy.created_at = ts(t0, 0);
    let mut e = base("fixture-lossy", 1, 0x300, ts(t0, 0));
    e.payload = Some(json!({"x": 4481891198857450.0f64, "y": -1.81996730402717e-179f64}));
    lossy.append(e);
    let lossy = seal_write(&segdir, lossy);
    let reread = Segment::read_from_file(segdir.join(lossy.filename())).unwrap();
    fs::write(
        dir.join("jilog-verify.txt"),
        format!("{} jilog_verify={}\n", lossy.filename(), reread.verify().unwrap()),
    )
    .unwrap();

    // Checksums for the Go test table.
    let mut sums = String::new();
    for s in [&a1, &a2, &b, &empty, &big, &lossy] {
        sums.push_str(&format!("{} {} {}\n", s.filename(), s.checksum, s.events.len()));
    }
    fs::write(dir.join("checksums.txt"), sums).unwrap();

    // Compact events JSON exactly as the CRC sees it (for byte-diffing).
    fs::write(dir.join("fixture-a-000001.events.json"), serde_json::to_vec(&a1.events).unwrap()).unwrap();

    // jilog query JSON output (query.rs:309-325) over two zones.
    let results: Vec<(String, Vec<Event>)> = vec![
        ("default".to_string(), a1.events.iter().rev().cloned().collect()),
        ("ops".to_string(), b.events.iter().rev().take(3).cloned().collect()),
    ];
    let flat: Vec<serde_json::Value> = results
        .iter()
        .flat_map(|(zone, events)| events.iter().map(move |e| json!({"zone": zone, "event": e})))
        .collect();
    fs::write(dir.join("query-json.golden"), serde_json::to_string_pretty(&flat).unwrap() + "\n").unwrap();

    // jilog query text output (query.rs:327-362) with patterns.
    let mut text = String::new();
    let patterns: Vec<String> = vec!["hook-*".into(), "sub-\"q\"".into()];
    let total: usize = results.iter().map(|(_, e)| e.len()).sum();
    text.push_str(&format!("Found {} event(s) since {}\n", total, "7d"));
    text.push_str(&format!("Subsystem filter: {:?}\n", patterns));
    text.push('\n');
    for (zone, events) in &results {
        text.push_str(&format!("Zone: {} ({} events)\n", zone, events.len()));
        for event in events {
            let ts = event.timestamp.format("%Y-%m-%d %H:%M:%S");
            let subsystem = event
                .payload
                .as_ref()
                .and_then(|p| p.get("subsystem"))
                .and_then(|v| v.as_str())
                .or_else(|| event.object_ref.as_deref().and_then(|r| r.strip_prefix("subsystem:")))
                .unwrap_or("?");
            let summary = event.payload.as_ref().and_then(|p| p.get("summary")).and_then(|s| s.as_str()).unwrap_or("");
            let class = format!("{:?}", event.event_class).to_lowercase();
            text.push_str(&format!("  [{ts}] {class:14} {subsystem:30} {summary}\n"));
        }
        text.push('\n');
    }
    fs::write(dir.join("query-text.golden"), text).unwrap();
    let none: Vec<String> = vec!["a\tb".into(), "\u{e9}".into()];
    fs::write(
        dir.join("query-text-empty.golden"),
        format!("No events found since {}{}.\n", "2026-01-01", format!(" matching {:?}", none)),
    )
    .unwrap();

    // v5 deterministic ids.
    let ns = Uuid::new_v5(&Uuid::NAMESPACE_URL, b"agentsview:ledger:event-id");
    let mut v5 = format!("namespace {}\n", ns);
    for (src, key) in [("av-0123", "diagnostic:disk-full:host-a"), ("host", "")] {
        let mut name = src.as_bytes().to_vec();
        name.push(0);
        name.extend_from_slice(key.as_bytes());
        v5.push_str(&format!("{:?} {:?} {}\n", src, key, Uuid::new_v5(&ns, &name)));
    }
    fs::write(dir.join("uuid-v5.txt"), v5).unwrap();

    // serde_json f64 formatting corpus: "<bits hex> <serde output>".
    let mut x: u64 = 0x9E3779B97F4A7C15;
    let mut floats = String::new();
    let mut n = 0;
    while n < 2000 {
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        let f = if n % 2 == 0 {
            f64::from_bits(x)
        } else {
            ((x % 10_000_000_000) as f64) / 10f64.powi((x % 25) as i32)
        };
        if !f.is_finite() {
            continue;
        }
        floats.push_str(&format!("{:016x} {}\n", f.to_bits(), serde_json::to_string(&f).unwrap()));
        n += 1;
    }
    fs::write(dir.join("serde-f64.txt"), floats).unwrap();
}
