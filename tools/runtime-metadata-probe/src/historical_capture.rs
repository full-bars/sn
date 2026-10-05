//! Collect an execution witness from a retained, read-only parent trie.
//!
//! The same pinned SDK function backs node `ProofProvider::execution_proof`.
//! Merely receiving a read proof does not grant completeness: this helper runs
//! the whole original block, retains code/heap and root-write paths, and then
//! subjects the exported job to the separate strict proof replay. It never
//! commits the speculative overlay or admits source semantics/finality/fees.
//! Callers must enclose execution in the owned subprocess deadline; a backend
//! accessor must not perform unbounded blocking I/O outside that process.

use super::*;
use sp_core::H256;
use sp_state_machine::{
    prove_execution_on_trie_backend, TrieBackend, TrieBackendBuilder, TrieBackendStorage,
};
use std::collections::BTreeMap;
use std::sync::{
    atomic::{AtomicBool, Ordering},
    Mutex,
};

#[cfg(target_os = "linux")]
use std::{
    fs::{File, OpenOptions},
    io::Read,
    os::{
        fd::AsRawFd,
        unix::fs::{MetadataExt, OpenOptionsExt},
    },
};

/// A bounded capture request omits proof nodes and obtains code from the actual
/// parent backend. Every selected body/header/code identity is caller-pinned.
pub const CAPTURE_SCHEMA: &str = "urnetwork-historical-execution-capture-v1";
pub const MAXIMUM_CAPTURE_REQUEST_BYTES: usize = 20 * 1024 * 1024;
pub const MAXIMUM_CAPTURE_REPORT_BYTES: usize = 104 * 1024 * 1024;

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CaptureRequest {
    pub schema: String,
    pub parent_header_hex: String,
    pub parent_hash: [u8; 32],
    pub child_header_hex: String,
    pub child_hash: [u8; 32],
    pub extrinsics_hex: Vec<String>,
    pub runtime_code_sha256: [u8; 32],
    pub runtime_code_blake2b_256: [u8; 32],
    pub execution_state_version: u8,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub observation_profile: Option<observer::ObservationProfile>,
}

/// Exact job JSON is preserved as bytes in a string, avoiding authority based
/// on another language's JSON re-encoding. The job digest is also in replay.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CaptureReport {
    pub schema: String,
    pub request_sha256: [u8; 32],
    pub sdk_revision: String,
    pub capture_method: String,
    pub backend_reads: usize,
    pub backend_read_bytes: usize,
    pub job_json: String,
    pub replay: HistoricalReport,
}

#[derive(Default)]
struct CapturedNodes {
    reads: usize,
    read_bytes: usize,
    bytes: usize,
    nodes: BTreeMap<H256, Vec<u8>>,
    failure: Option<String>,
}

/// The only parent-state surface is a read accessor. Keep an independent sticky
/// failure because SDK trie helpers may suppress an error and return old roots.
struct CaptureStorage<'a, S> {
    original: &'a S,
    canceled: &'a AtomicBool,
    captured: &'a Mutex<CapturedNodes>,
}

impl<S: TrieBackendStorage<Blake2Hasher>> TrieBackendStorage<Blake2Hasher>
    for CaptureStorage<'_, S>
{
    fn get(&self, key: &H256, prefix: (&[u8], Option<u8>)) -> Result<Option<Vec<u8>>, String> {
        let before = || -> Result<(), String> {
            let mut state = self
                .captured
                .lock()
                .map_err(|_| "historical capture lock poisoned")?;
            if let Some(error) = &state.failure {
                return Err(error.clone());
            }
            if self.canceled.load(Ordering::Acquire) {
                return Err("historical capture canceled".to_owned());
            }
            state.reads = state
                .reads
                .checked_add(1)
                .ok_or("historical capture read overflow")?;
            if state.reads > 65536 {
                return Err("historical capture read count bound".to_owned());
            }
            Ok(())
        };
        let outcome = (|| {
            before()?;
            let value = self.original.get(key, prefix)?;
            let raw = value
                .as_ref()
                .ok_or("historical capture parent trie node is absent")?;
            if raw.is_empty() || raw.len() > MAXIMUM_CODE_BYTES || H256(blake2_256(raw)) != *key {
                return Err(
                    "historical capture trie node size or content identity differs".to_owned(),
                );
            }
            // Returned bytes can establish an actual contradiction. Preserve
            // that refusal even if the owner canceled during this read.
            if self.canceled.load(Ordering::Acquire) {
                return Err("historical capture canceled".to_owned());
            }
            let mut state = self
                .captured
                .lock()
                .map_err(|_| "historical capture lock poisoned")?;
            state.read_bytes = state
                .read_bytes
                .checked_add(raw.len())
                .ok_or("historical capture I/O overflow")?;
            if state.read_bytes > 64 * 1024 * 1024 {
                return Err("historical capture cumulative read byte bound".to_owned());
            }
            if !state.nodes.contains_key(key) {
                if state.nodes.len() >= 8192 || raw.len() > MAXIMUM_PROOF_BYTES - state.bytes {
                    return Err("historical capture retained proof bound".to_owned());
                }
                state.bytes += raw.len();
                state.nodes.insert(*key, raw.clone());
            }
            Ok(value)
        })();
        if let Err(error) = &outcome {
            if let Ok(mut state) = self.captured.lock() {
                state.failure.get_or_insert_with(|| error.clone());
            }
        }
        outcome
    }
}

/// Inspect the retained accessor cause before SDK trie error translation. A
/// canceled read must not become an invalid-root claim, and an already
/// observed backend refusal must not be erased by a later cancellation.
fn check_failure(captured: &Mutex<CapturedNodes>) -> Result<(), ProbeError> {
    let state = captured
        .lock()
        .map_err(|_| ProbeError::new("historical capture lock poisoned"))?;
    if let Some(error) = &state.failure {
        return Err(ProbeError::new(error));
    }
    Ok(())
}

fn check_cancellation(canceled: &AtomicBool) -> Result<(), ProbeError> {
    if canceled.load(Ordering::Acquire) {
        return Err(ProbeError::new("historical capture canceled"));
    }
    Ok(())
}

fn check(canceled: &AtomicBool, captured: &Mutex<CapturedNodes>) -> Result<(), ProbeError> {
    check_failure(captured)?;
    check_cancellation(canceled)
}

/// Instance-local observation of completed work. The public entry has no
/// observer; tests use these boundaries without timers or global hooks.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum CapturePhase {
    Code,
    Heap,
    Execution,
    Root,
    Replay,
    Report,
}

/// Borrow an archive node's retained parent `state.as_trie_backend()`. This
/// uses only its content-addressed read surface, never its transaction writer,
/// runtime override provider, signer, offchain extensions or network services.
pub fn capture_historical_on_backend<S: TrieBackendStorage<Blake2Hasher>>(
    raw: &[u8],
    parent_backend: &TrieBackend<S, Blake2Hasher>,
    canceled: &AtomicBool,
) -> Result<Vec<u8>, ProbeError> {
    capture_historical_on_backend_observed(raw, parent_backend, canceled, |_| {})
}

pub(super) fn capture_historical_on_backend_observed<S: TrieBackendStorage<Blake2Hasher>>(
    raw: &[u8],
    parent_backend: &TrieBackend<S, Blake2Hasher>,
    canceled: &AtomicBool,
    mut observed: impl FnMut(CapturePhase),
) -> Result<Vec<u8>, ProbeError> {
    let captured = Mutex::new(CapturedNodes::default());
    check(canceled, &captured)?;
    if raw.is_empty() || raw.len() > MAXIMUM_CAPTURE_REQUEST_BYTES {
        return Err(ProbeError::new("historical capture request byte bound"));
    }
    let request: CaptureRequest = serde_json::from_slice(raw)
        .map_err(|e| ProbeError::new(format!("historical capture request JSON: {e}")))?;
    if request.schema != CAPTURE_SCHEMA || request.extrinsics_hex.len() > 16384 {
        return Err(ProbeError::new(
            "historical capture schema or body item bound",
        ));
    }
    let parent: NativeHeader = scale_exact(
        "capture parent header",
        &hex_bytes(
            "capture parent header",
            &request.parent_header_hex,
            MAXIMUM_HEADER_BYTES,
        )?,
    )?;
    let child: NativeHeader = scale_exact(
        "capture child header",
        &hex_bytes(
            "capture child header",
            &request.child_header_hex,
            MAXIMUM_HEADER_BYTES,
        )?,
    )?;
    if parent.hash().0 != request.parent_hash
        || child.hash().0 != request.child_hash
        || child.parent_hash() != &parent.hash()
        || parent.number().checked_add(1) != Some(*child.number())
        || parent.state_root() != parent_backend.root()
        || parent.digest().logs.len() > 64
        || child.digest().logs.len() > 64
    {
        return Err(ProbeError::new(
            "historical capture parent/child/backend identity differs",
        ));
    }
    let state_version = StateVersion::try_from(request.execution_state_version)
        .map_err(|_| ProbeError::new("historical capture state version unsupported"))?;
    let mut body = Vec::new();
    let mut body_bytes = 0;
    for encoded in &request.extrinsics_hex {
        check(canceled, &captured)?;
        let bytes = hex_bytes("capture extrinsic", encoded, MAXIMUM_BLOCK_BYTES)?;
        body_bytes += bytes.len();
        if body_bytes > MAXIMUM_BLOCK_BYTES {
            return Err(ProbeError::new("historical capture block byte bound"));
        }
        body.push(scale_exact::<OpaqueExtrinsic>("capture extrinsic", &bytes)?);
    }
    if BlakeTwo256::ordered_trie_root(body.iter().map(Encode::encode).collect(), state_version)
        != *child.extrinsics_root()
    {
        return Err(ProbeError::new(
            "historical capture extrinsics root differs",
        ));
    }
    let backend = TrieBackendBuilder::new(
        CaptureStorage {
            original: parent_backend.backend_storage(),
            canceled,
            captured: &captured,
        },
        *parent.state_root(),
    )
    .build();
    let code = backend.storage(well_known_keys::CODE);
    observed(CapturePhase::Code);
    check_failure(&captured)?;
    let code = code
        .map_err(|e| ProbeError::new(format!("historical capture parent code: {e}")))?
        .ok_or_else(|| ProbeError::new("historical capture parent code absent"))?;
    if code.len() > MAXIMUM_CODE_BYTES
        || sha2_256(&code) != request.runtime_code_sha256
        || blake2_256(&code) != request.runtime_code_blake2b_256
    {
        return Err(ProbeError::new(
            "historical capture parent runtime code differs",
        ));
    }
    check(canceled, &captured)?;
    let heap_pages = backend.storage(well_known_keys::HEAP_PAGES);
    observed(CapturePhase::Heap);
    check_failure(&captured)?;
    let heap_pages = heap_pages
        .map_err(|e| ProbeError::new(format!("historical capture heap proof: {e}")))?
        .map(|bytes| scale_exact::<u64>("capture heap pages", &bytes))
        .transpose()?;
    check(canceled, &captured)?;
    let wasm = sp_maybe_compressed_blob::decompress(&code, MAXIMUM_EXPANDED_CODE_BYTES)
        .map_err(|e| ProbeError::new(format!("historical capture code decompression: {e}")))?;
    memory_bound(&wasm, heap_pages)?;
    let wrapped = WrappedRuntimeCode(code.as_slice().into());
    let runtime = RuntimeCode {
        code_fetcher: &wrapped,
        heap_pages,
        hash: request.runtime_code_blake2b_256.to_vec(),
    };
    let executor = WasmExecutor::<hosts::HistoricalHostFunctions>::builder()
        .with_allow_missing_host_functions(true)
        .with_onchain_heap_alloc_strategy(HeapAllocStrategy::Dynamic {
            maximum_pages: Some(1024),
        })
        .with_offchain_heap_alloc_strategy(HeapAllocStrategy::Dynamic {
            maximum_pages: Some(1024),
        })
        .build();
    let mut execution_header = child.clone();
    while execution_header
        .digest()
        .logs
        .last()
        .is_some_and(|item| item.as_seal().is_some())
    {
        execution_header.digest_mut().pop();
    }
    if execution_header
        .digest()
        .logs
        .iter()
        .any(|item| item.as_seal().is_some())
    {
        return Err(ProbeError::new("historical capture seal ordering differs"));
    }
    let block = Block {
        header: execution_header,
        extrinsics: body,
    };
    let mut overlay = OverlayedChanges::<Blake2Hasher>::default();
    let mut extensions = Extensions::default();
    extensions.register(hosts::HistoricalBudget(hosts::Budget::default()));
    check(canceled, &captured)?;
    // This is the exact pinned SDK execution-proof primitive, not a caller
    // claim that an arbitrary state_getReadProof key list happened to suffice.
    let execution = std::panic::catch_unwind(AssertUnwindSafe(|| {
        prove_execution_on_trie_backend(
            &backend,
            &mut overlay,
            &executor,
            "Core_execute_block",
            &block.encode(),
            &runtime,
            &mut extensions,
        )
    }));
    observed(CapturePhase::Execution);
    check_failure(&captured)?;
    let (output, proof) = execution
        .map_err(|_| ProbeError::new("historical capture execution panicked on parent proof"))?
        .map_err(|e| ProbeError::new(format!("historical capture execution refused: {e}")))?;
    if !output.is_empty()
        || overlay.transaction_depth() != 0
        || extensions
            .get_mut(TypeId::of::<hosts::HistoricalBudget>())
            .and_then(|value| value.downcast_mut::<hosts::HistoricalBudget>())
            .is_none_or(|value| value.0.depth != 0)
    {
        return Err(ProbeError::new(
            "historical capture output or transaction balance differs",
        ));
    }
    check(canceled, &captured)?;
    // Record paths needed to materialize writes too. A runtime need not ask
    // the root itself. A suppressed SDK read/root error remains sticky above,
    // and strict replay below independently rejects incomplete write paths.
    let root = std::panic::catch_unwind(AssertUnwindSafe(|| {
        overlay.storage_root(&backend, state_version).0
    }));
    observed(CapturePhase::Root);
    check_failure(&captured)?;
    let root =
        root.map_err(|_| ProbeError::new("historical capture root materialization panicked"))?;
    if root != *child.state_root() {
        return Err(ProbeError::new(
            "historical capture child state root differs",
        ));
    }
    check(canceled, &captured)?;
    drop(backend);
    let state = captured
        .into_inner()
        .map_err(|_| ProbeError::new("historical capture lock poisoned"))?;
    let mut nodes: BTreeSet<Vec<u8>> = state.nodes.into_values().collect();
    nodes.extend(proof.into_iter_nodes());
    if nodes.len() > 8192 || nodes.iter().map(Vec::len).sum::<usize>() > MAXIMUM_PROOF_BYTES {
        return Err(ProbeError::new("historical capture combined proof bound"));
    }
    let job = HistoricalJob {
        schema: HISTORICAL_SCHEMA.to_owned(),
        parent_header_hex: request.parent_header_hex,
        parent_hash: request.parent_hash,
        child_header_hex: request.child_header_hex,
        child_hash: request.child_hash,
        extrinsics_hex: request.extrinsics_hex,
        runtime_code_hex: format!("0x{}", hex::encode(code)),
        runtime_code_sha256: request.runtime_code_sha256,
        runtime_code_blake2b_256: request.runtime_code_blake2b_256,
        execution_state_version: request.execution_state_version,
        proof_nodes_hex: nodes
            .iter()
            .map(|node| format!("0x{}", hex::encode(node)))
            .collect(),
        observation_profile: request.observation_profile,
    };
    let job_json = serde_json::to_string(&job)
        .map_err(|e| ProbeError::new(format!("historical capture job JSON: {e}")))?;
    check_cancellation(canceled)?;
    let replay = replay_historical_json(job_json.as_bytes());
    observed(CapturePhase::Replay);
    // A completed strict replay refusal is evidence already obtained; owner
    // cancellation must not erase it or publish a successful report instead.
    let replay = replay?;
    check_cancellation(canceled)?;
    let report = CaptureReport {
        schema: CAPTURE_SCHEMA.to_owned(),
        request_sha256: sha2_256(raw),
        sdk_revision: POLKADOT_SDK_REVISION.to_owned(),
        capture_method: "pinned-sdk-execution-proof-plus-strict-replay".to_owned(),
        backend_reads: state.reads,
        backend_read_bytes: state.read_bytes,
        job_json,
        replay: serde_json::from_slice(&replay)
            .map_err(|e| ProbeError::new(format!("historical capture replay JSON: {e}")))?,
    };
    let encoded = serde_json::to_vec(&report)
        .map_err(|e| ProbeError::new(format!("historical capture report JSON: {e}")))?;
    observed(CapturePhase::Report);
    if encoded.len() > MAXIMUM_CAPTURE_REPORT_BYTES {
        return Err(ProbeError::new("historical capture report byte bound"));
    }
    check_cancellation(canceled)?;
    Ok(encoded)
}

/// A retained directory is an interchange adapter for raw trie nodes exported
/// by an archive node. Files are named by lowercase Blake2-256 and opened only
/// through its held directory descriptor. It is not a node database parser;
/// embedded node users borrow the actual SDK TrieBackend above directly.
#[cfg(target_os = "linux")]
struct NodeDirectory<'a> {
    root: &'a File,
}

#[cfg(target_os = "linux")]
impl TrieBackendStorage<Blake2Hasher> for NodeDirectory<'_> {
    fn get(&self, key: &H256, _prefix: (&[u8], Option<u8>)) -> Result<Option<Vec<u8>>, String> {
        let path = format!(
            "/proc/self/fd/{}/{}",
            self.root.as_raw_fd(),
            hex::encode(key.0)
        );
        let mut file = OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
            .open(path)
            .map_err(|e| format!("historical capture node read: {e}"))?;
        let before = file
            .metadata()
            .map_err(|e| format!("historical capture node stat: {e}"))?;
        if !before.is_file()
            || before.len() == 0
            || before.len() > MAXIMUM_CODE_BYTES as u64
            || before.nlink() != 1
        {
            return Err("historical capture node is not a bounded unique regular file".to_owned());
        }
        let mut raw = Vec::with_capacity(before.len() as usize);
        (&mut file)
            .take((MAXIMUM_CODE_BYTES + 1) as u64)
            .read_to_end(&mut raw)
            .map_err(|e| format!("historical capture node payload: {e}"))?;
        let after = file
            .metadata()
            .map_err(|e| format!("historical capture node closing stat: {e}"))?;
        let identity = |value: &std::fs::Metadata| {
            (
                value.dev(),
                value.ino(),
                value.len(),
                value.mode(),
                value.nlink(),
                value.mtime(),
                value.mtime_nsec(),
                value.ctime(),
                value.ctime_nsec(),
            )
        };
        if identity(&before) != identity(&after)
            || raw.len() != before.len() as usize
            || blake2_256(&raw) != key.0
        {
            return Err(
                "historical capture node changed or differs from its content address".to_owned(),
            );
        }
        Ok(Some(raw))
    }
}

/// The standalone worker receives an already retained directory from its Go
/// owner. Directory replacement cannot retarget it; each payload additionally
/// authenticates its own content address and the request's parent state root.
#[cfg(target_os = "linux")]
pub fn capture_historical_directory_json(raw: &[u8], root: &File) -> Result<Vec<u8>, ProbeError> {
    if raw.is_empty() || raw.len() > MAXIMUM_CAPTURE_REQUEST_BYTES {
        return Err(ProbeError::new("historical capture request byte bound"));
    }
    let request: CaptureRequest = serde_json::from_slice(raw)
        .map_err(|e| ProbeError::new(format!("historical capture request JSON: {e}")))?;
    let parent: NativeHeader = scale_exact(
        "capture parent header",
        &hex_bytes(
            "capture parent header",
            &request.parent_header_hex,
            MAXIMUM_HEADER_BYTES,
        )?,
    )?;
    if !root
        .metadata()
        .map_err(|e| ProbeError::new(format!("historical capture node directory: {e}")))?
        .is_dir()
    {
        return Err(ProbeError::new(
            "historical capture node directory is absent",
        ));
    }
    let backend = TrieBackendBuilder::new(NodeDirectory { root }, *parent.state_root()).build();
    capture_historical_on_backend(raw, &backend, &AtomicBool::new(false))
}
