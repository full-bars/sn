//! Real original-Wasm fixture for the Go execution/accounting boundary. The
//! program computes normalization and recipients; the host never accepts a
//! supplied amount report. These bytes carry no deployed-runtime authority.

use super::*;
use std::{fs::OpenOptions, io::Write, os::unix::fs::OpenOptionsExt, path::Path};

fn key(pallet: &[u8], item: &[u8], subnet: bool) -> Vec<u8> {
    let mut value = [
        sp_core::hashing::twox_128(pallet),
        sp_core::hashing::twox_128(item),
    ]
    .concat();
    if subnet {
        value.extend_from_slice(&25u16.to_le_bytes());
    }
    value
}

fn segment(address: u32, bytes: &[u8]) -> String {
    let escaped = bytes
        .iter()
        .map(|byte| format!("\\{byte:02x}"))
        .collect::<String>();
    format!("(data (i32.const {address}) \"{escaped}\")")
}

fn span(address: u32, length: usize) -> u64 {
    (length as u64) << 32 | u64::from(address)
}

fn words(values: &[u64]) -> Vec<u8> {
    values
        .iter()
        .flat_map(|value| value.to_le_bytes())
        .collect()
}

fn fixture() -> HistoricalJob {
    let drains = [
        key(b"SubtensorModule", b"PendingServerEmission", true),
        key(b"SubtensorModule", b"PendingValidatorEmission", true),
        key(b"SubtensorModule", b"PendingRootAlphaDivs", true),
    ];
    let phase = key(b"System", b"ExecutionPhase", false);
    let events = key(b"System", b"Events", false);
    let provider = b"synthetic-native-provider-credit";
    let owner = b"synthetic-native-owner-recycle";
    let epoch = b"synthetic-native-epoch";
    // Synthetic event indices are deliberately fixture-local. The consumer
    // join below checks accounting; public metadata traversal has its own tests.
    let mut event = vec![2, 7, 250, 25, 0, 8];
    event.extend_from_slice(&words(&[9, 89]));
    event.push(0);
    let zero = vec![0; 8];
    let mut declarations =
        "(import \"env\" \"ext_storage_append_version_1\" (func $append (param i64 i64)))"
            .to_owned();
    let data: Vec<(u32, Vec<u8>)> = vec![
        (1000, drains[0].clone()),
        (1100, drains[1].clone()),
        (1200, drains[2].clone()),
        (1400, events.clone()),
        (1500, provider.to_vec()),
        (1600, owner.to_vec()),
        (1700, epoch.to_vec()),
        (1800, zero),
        (4000, vec![25, 0]),
        (4008, words(&[10, 3])),
        (4100, words(&[429496729, 3865470567])),
        (4120, words(&[1 << 32, 0])),
        (4180, vec![0, 0, 1, 0]),
        (4200, vec![0x11; 32]),
        (4232, vec![0x22; 32]),
        (4280, words(&[20, 21])),
        (4500, vec![0x33; 32]),
        (4532, vec![0x34; 32]),
        (4800, event.clone()),
    ];
    for (address, bytes) in data {
        declarations.push_str(&segment(address, &bytes));
    }
    declarations.push_str(&format!(r#"
        (func $drain (export "native_drain") (param $key i64) (result i64) (call $get (local.get $key)))
        (func $epoch (export "native_epoch")
            (i64.store (i32.const 4048) (i64.add (i64.add (i64.load (i32.const 4024)) (i64.load (i32.const 4032))) (i64.load (i32.const 4040))))
            (i64.store (i32.const 4140) (i64.div_u (i64.shl (i64.load (i32.const 4100)) (i64.const 32)) (i64.const 8589934592)))
            (i64.store (i32.const 4148) (i64.div_u (i64.shl (i64.load (i32.const 4108)) (i64.const 32)) (i64.const 8589934592)))
            (i64.store (i32.const 4160) (i64.shr_u (i64.mul (i64.load (i32.const 4048)) (i64.load (i32.const 4140))) (i64.const 32)))
            (i64.store (i32.const 4168) (i64.shr_u (i64.mul (i64.load (i32.const 4048)) (i64.load (i32.const 4148))) (i64.const 32)))
            (call $set (i64.const {epoch_key}) (i64.const {epoch_value})))
        (func $emission (export "native_emission")
            (i64.store (i32.const 4806) (i64.load (i32.const 4160)))
            (i64.store (i32.const 4814) (i64.load (i32.const 4168)))
            (call $append (i64.const {events_key}) (i64.const {event_value})))
        (func $provider (export "native_provider")
            (memory.copy (i32.const 4300) (i32.const 4200) (i32.const 32))
            (memory.copy (i32.const 4332) (i32.const 4500) (i32.const 32))
            (i64.store (i32.const 4368) (i64.load (i32.const 4160)))
            (i64.store (i32.const 4376) (i64.const 3))
            (i64.store (i32.const 4384) (i64.sub (i64.load (i32.const 4368)) (i64.load (i32.const 4376))))
            (call $set (i64.const {provider_key}) (i64.const {provider_value})))
        (func $owner (export "native_owner")
            (memory.copy (i32.const 4300) (i32.const 4232) (i32.const 32))
            (memory.copy (i32.const 4332) (i32.const 4532) (i32.const 32))
            (i64.store (i32.const 4368) (i64.load (i32.const 4168)))
            (i64.store (i32.const 4392) (i64.load (i32.const 4368)))
            (call $set (i64.const {owner_key}) (i64.const {owner_value})))"#,
        epoch_key=span(1700,epoch.len()), epoch_value=span(4048,8), events_key=span(1400,events.len()), event_value=span(4800,event.len()), provider_key=span(1500,provider.len()),provider_value=span(4368,24),owner_key=span(1600,owner.len()),owner_value=span(4392,8)));
    let mut body = String::new();
    for index in 0..3 {
        body.push_str(&format!("(local.set $n (i32.wrap_i64 (call $drain (i64.const {})))) (if (i32.ne (i32.load8_u (local.get $n)) (i32.const 1)) (then unreachable)) (if (i32.ne (i32.load8_u offset=1 (local.get $n)) (i32.const 32)) (then unreachable)) (i64.store (i32.const {}) (i64.load offset=2 (local.get $n))) (call $set (i64.const {}) (i64.const {}))",span(1000+index as u32*100,drains[index].len()),4024+index*8,span(1000+index as u32*100,drains[index].len()),span(1800,8)));
    }
    body.push_str("(call $epoch) (call $emission) (call $provider) (call $owner)");
    let code = wasm(&declarations, &body);
    let mut initial = parent_storage(&code);
    for (index, drain) in drains.iter().enumerate() {
        initial
            .top
            .insert(drain.clone(), words(&[[100, 100, 0][index]]));
    }
    initial.top.insert(phase, vec![2]);
    initial.top.insert(events.clone(), vec![0]);
    let backing = TestExternalities::<Blake2Hasher>::new_with_code_and_state(
        &code,
        initial.clone(),
        StateVersion::V1,
    );
    let (nodes, parent_root) = backing.into_raw_snapshot();
    let parent = NativeHeader::new(
        100,
        H256::repeat_byte(2),
        parent_root,
        H256::repeat_byte(1),
        Digest::default(),
    );
    let mut expected = initial;
    for drain in drains {
        expected.top.insert(drain, words(&[0]));
    }
    expected.top.insert(epoch.to_vec(), words(&[200]));
    expected.top.insert(provider.to_vec(), words(&[9, 3, 6]));
    expected.top.insert(owner.to_vec(), words(&[89]));
    expected.top.insert(events, [vec![4], event].concat());
    let backing = TestExternalities::<Blake2Hasher>::new_with_code_and_state(
        &code,
        expected,
        StateVersion::V1,
    );
    let extrinsics: Vec<Vec<u8>> = Vec::new();
    let child = NativeHeader::new(
        101,
        BlakeTwo256::ordered_trie_root(extrinsics.clone(), StateVersion::V1),
        *backing.backend.root(),
        parent.hash(),
        Digest::default(),
    );
    let mut profile = observation_profile(&code, "native_drain", "native-drain");
    profile.schema = "urnetwork-original-wasm-native-observation-v2".to_owned();
    let fields = |items: &[(&str, u32, u32)]| {
        items
            .iter()
            .map(|(name, address, bytes)| observer::MemoryCapture {
                name: (*name).to_owned(),
                address: *address,
                global: None,
                dereference_offsets: Vec::new(),
                bytes: *bytes,
                repeat: None,
            })
            .collect()
    };
    profile.rules[0].memory = fields(&[
        ("netuid", 4000, 2),
        ("subnet-registered", 4008, 8),
        ("subnet-generation", 4016, 8),
    ]);
    for (export, purpose, items) in [
        (
            "native_epoch",
            "native-epoch",
            vec![
                ("netuid", 4000, 2),
                ("total-alpha", 4048, 8),
                ("incentive-q32", 4100, 16),
                ("dividends-q32", 4120, 16),
                ("normalized-q32", 4140, 16),
                ("emission", 4160, 16),
                ("uids", 4180, 4),
                ("hotkeys", 4200, 64),
                ("registered", 4280, 16),
            ],
        ),
        (
            "native_emission",
            "native-emission",
            vec![("netuid", 4000, 2)],
        ),
        (
            "native_provider",
            "native-miner-credit",
            vec![
                ("netuid", 4000, 2),
                ("hotkey", 4300, 32),
                ("coldkey", 4332, 32),
                ("gross", 4368, 8),
                ("captured", 4376, 8),
                ("liquid", 4384, 8),
            ],
        ),
        (
            "native_owner",
            "native-owner-recycle",
            vec![
                ("netuid", 4000, 2),
                ("hotkey", 4300, 32),
                ("coldkey", 4332, 32),
                ("gross", 4368, 8),
                ("recycled", 4392, 8),
            ],
        ),
    ] {
        let mut rule = observation_profile(&code, export, purpose).rules.remove(0);
        rule.memory = fields(&items);
        profile.rules.push(rule);
    }
    HistoricalJob {
        schema: HISTORICAL_SCHEMA.to_owned(),
        parent_header_hex: encoded(&parent.encode()),
        parent_hash: parent.hash().0,
        child_header_hex: encoded(&child.encode()),
        child_hash: child.hash().0,
        extrinsics_hex: Vec::new(),
        runtime_code_hex: encoded(&code),
        runtime_code_sha256: sha2_256(&code),
        runtime_code_blake2b_256: blake2_256(&code),
        execution_state_version: 1,
        proof_nodes_hex: nodes
            .into_iter()
            .map(|(_, (value, _))| encoded(&value))
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect(),
        observation_profile: Some(profile),
    }
}

#[test]
fn historical_native_execution_exports_actual_original_program_for_go_consumer() {
    let job = fixture();
    let report = run(&job).expect("actual original native fixture refused");
    assert!(
        report.post_state_reproduced
            && !report.runtime_admitted
            && report.native_fee_debit.is_none()
    );
    let records = &report.hook_observations.as_ref().unwrap().observations;
    assert_eq!(records.len(), 7);
    assert_eq!(
        records[0].storage_return.as_ref().unwrap().value_hex,
        Some(encoded(&words(&[100])))
    );
    assert_eq!(
        records[3]
            .native
            .as_ref()
            .unwrap()
            .memory
            .iter()
            .find(|value| value.name == "emission")
            .unwrap()
            .bytes_hex,
        encoded(&words(&[9, 89]))
    );
    if let Some(directory) = std::env::var_os("URNETWORK_NATIVE_EXECUTION_FIXTURE_OUT") {
        let directory = Path::new(&directory);
        assert!(directory.is_absolute() && directory.is_dir());
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .mode(0o600)
            .open(directory.join("native-job.json"))
            .expect("exclusive original fixture output");
        file.write_all(&serde_json::to_vec(&job).unwrap())
            .expect("complete original fixture output");
        file.sync_all().expect("sync original fixture output");
    }
}
