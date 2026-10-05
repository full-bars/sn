#!/usr/bin/env python3
"""Capture the fixed R48 chain window from the owned LAN archive RPC."""

import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time
import urllib.request

HERE = Path(__file__).resolve().parent
SN = HERE.parents[3]
sys.path.insert(0, str(SN / "sim-testnet/peerreview/verify"))
from keccak import keccak256  # Independent peer-review Keccak implementation.

RPC = os.environ.get("SN_RPC_URL", "http://192.168.1.162:9944")
COORDINATOR = "0x8e7d2f9a77fec95c7e4875b0bd858d5de2b6def8"
VAULT = "0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e"
BLOCKS = [8093074, 8093315, 8093328, 8093374, 8093375, 8093377, 8093380]
EVENT_NAMES = {"OperatorEpochFinalized", "OperatorRootCommitted", "RootMissed", "EmissionCaptured"}


def rpc(method, params):
    wire = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode()
    request = urllib.request.Request(RPC, data=wire, headers={"Content-Type": "application/json"})
    deadline = time.monotonic() + 300
    attempt = 0
    while True:
        attempt += 1
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                answer = json.load(response)
            if "error" in answer:
                raise RuntimeError(f"{method}: {answer['error']}")
            return answer["result"]
        except (OSError, TimeoutError) as error:
            if time.monotonic() >= deadline:
                raise RuntimeError(f"{method} failed after {attempt} attempts") from error
            time.sleep(min(attempt, 5))


def event_topics():
    source = (SN / "sim-testnet/contracts_gen.go").read_text()
    result = {}
    for abi_name in ("CoordinatorABI", "SettlementVaultABI"):
        match = re.search(r"const " + abi_name + r" = `([^`]+)`", source)
        if match is None:
            raise RuntimeError(f"missing {abi_name}")
        for event in json.loads(match.group(1)):
            if event.get("type") != "event" or event["name"] not in EVENT_NAMES:
                continue
            signature = event["name"] + "(" + ",".join(item["type"] for item in event["inputs"]) + ")"
            result["0x" + keccak256(signature.encode()).hex()] = event["name"]
    assert set(result.values()) == EVENT_NAMES
    return result


def decode_event(log, topics):
    name = topics.get(log["topics"][0])
    if name is None:
        raise RuntimeError(f"unknown coordinator/vault event {log['topics'][0]}")
    values = [int(log["data"][2 + 64 * index:2 + 64 * (index + 1)], 16)
              for index in range((len(log["data"]) - 2) // 64)]
    row = {"name": name, "epoch": int(log["topics"][1], 16),
           "no_id": int(log["topics"][2], 16), "block": int(log["blockNumber"], 16),
           "transaction_hash": log["transactionHash"], "log_index": int(log["logIndex"], 16)}
    if name == "OperatorRootCommitted":
        row.update(payout_root="0x" + log["data"][2:66], artifact_hash="0x" + log["data"][66:130],
                   committer="0x" + log["data"][154:194])
    elif name == "OperatorEpochFinalized":
        row["root_present"] = bool(values[0])
    elif name == "RootMissed":
        row["carried_rao"] = str(values[0])
    elif name == "EmissionCaptured":
        row["pool_hotkey"] = log["topics"][3]
        row["amount_rao"] = str(values[0])
    return row


def file_sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    chain_id = int(rpc("eth_chainId", []), 16)
    genesis = rpc("chain_getBlockHash", [0])
    finalized_hash = rpc("chain_getFinalizedHead", [])
    finalized_number = int(rpc("chain_getHeader", [finalized_hash])["number"], 16)
    assert chain_id == 945 and genesis == "0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105"
    assert finalized_number >= max(BLOCKS)
    blocks = {str(number): rpc("eth_getBlockByNumber", [hex(number), False]) for number in BLOCKS}
    assert all(int(blocks[str(number)]["number"], 16) == number for number in BLOCKS)
    assert blocks["8093328"]["hash"] == "0x9dc99854fbdd929b3df3f27d8fbb42c52f7aaf031af17f48ade76c40d687b730"
    topics = event_topics()
    logs = []
    for address in (COORDINATOR, VAULT):
        logs.extend(rpc("eth_getLogs", [{"fromBlock": hex(8092774), "toBlock": hex(8093380), "address": address}]))
    logs.sort(key=lambda row: (int(row["blockNumber"], 16), int(row["transactionIndex"], 16), int(row["logIndex"], 16)))
    events = [decode_event(log, topics) for log in logs]
    receipts = {}
    for transaction_hash in sorted({log["transactionHash"] for log in logs}):
        receipt = rpc("eth_getTransactionReceipt", [transaction_hash])
        assert receipt and int(receipt["status"], 16) == 1
        block_number = int(receipt["blockNumber"], 16)
        if str(block_number) not in blocks:
            blocks[str(block_number)] = rpc("eth_getBlockByNumber", [hex(block_number), False])
        assert receipt["blockHash"] == blocks[str(block_number)]["hash"]
        receipts[transaction_hash] = {"block_number": int(receipt["blockNumber"], 16),
                                      "block_hash": receipt["blockHash"], "status": int(receipt["status"], 16),
                                      "transaction_index": int(receipt["transactionIndex"], 16)}
    result = {
        "schema": "urnetwork-r48-lan-chain-capture-v1",
        "captured_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "rpc": RPC, "chain_id": chain_id, "genesis_hash": genesis,
        "finalized_at_capture": {"number": finalized_number, "hash": finalized_hash},
        "addresses": {"coordinator": COORDINATOR, "settlement_vault": VAULT},
        "blocks": blocks, "event_topics": topics, "logs": logs, "decoded_events": events,
        "receipts": receipts,
        "terminal_local_evidence": {
            "result_sha256": file_sha(SN / "sim-testnet/runs/ur-subnet-testnet-v1-attempt-4/runs/20260926T202718.915754659Z-release-1.0/result.json"),
            "attempt_sha256": file_sha(SN / "sim-testnet/runs/ur-subnet-testnet-v1-attempt-4/campaign-attempts/release-1.0.recovery.48.evidence.json"),
        },
    }
    output = HERE / "onchain.json"
    output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
    print(output, file_sha(output), "events", len(events), "receipts", len(receipts))


if __name__ == "__main__":
    main()
