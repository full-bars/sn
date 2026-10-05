//! Read-only complete-witness collector. Its parent owns the retained node
//! directory on fd4, exact executable/input pins, output bounds and deadline.

#![deny(unsafe_code)]

use runtime_metadata_probe::historical::capture::{
    capture_historical_directory_json, MAXIMUM_CAPTURE_REQUEST_BYTES,
};
use std::{
    fs::File,
    io::{Read, Write},
};

fn main() {
    if std::env::args().skip(1).collect::<Vec<_>>() != ["--historical-proof-capture-v1"] {
        eprintln!("runtime-historical-capture: fixed invocation argument required");
        std::process::exit(1);
    }
    let mut raw = Vec::new();
    let result = (|| {
        let root = File::open("/proc/self/fd/4").map_err(|e| e.to_string())?;
        std::io::stdin()
            .take((MAXIMUM_CAPTURE_REQUEST_BYTES + 1) as u64)
            .read_to_end(&mut raw)
            .map_err(|e| e.to_string())?;
        capture_historical_directory_json(&raw, &root).map_err(|e| e.to_string())
    })();
    match result {
        Ok(report) => {
            if std::io::stdout().write_all(&report).is_err() {
                std::process::exit(1);
            }
        }
        Err(error) => {
            eprintln!("runtime-historical-capture: {error}");
            std::process::exit(1);
        }
    }
}
