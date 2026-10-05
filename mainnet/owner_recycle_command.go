// Recycle commands observe, plan and retain public handoffs. They expose neither
// a signing device/private key nor an author_submitExtrinsic or service route.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// Public dispatcher keeps reads separate from offline custody operations.
func runOwnerRecycleCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "owner-recycle requires observe, plan, reserve, export, inspect-request, ledger-plan, import, status or reconcile")
		return 2
	}
	var value any
	var err error
	switch args[0] {
	case "observe":
		value, err = ownerRecycleObserveCommand(ctx, args[1:], stderr)
	case "plan":
		flags := flag.NewFlagSet("owner-recycle plan", flag.ContinueOnError)
		flags.SetOutput(stderr)
		inputPath := flags.String("input", "", "private JSON with reviewed action, birth observation, metadata and route")
		if parseErr := flags.Parse(args[1:]); parseErr != nil || flags.NArg() != 0 || *inputPath == "" {
			err = errors.New("recycle plan requires --input FILE")
		} else {
			var raw []byte
			raw, _, err = readBootstrapRootFile(ctx, *inputPath, ownerSigningRequestLimit)
			var input ownerRecyclePlanInput
			if err == nil {
				err = decodePlanJson(raw, &input)
			}
			if err == nil {
				value, err = prepareOwnerRecyclePlan(input)
			}
		}
	case "inspect-request", "ledger-plan":
		value, err = ownerRecycleRequestCommand(ctx, args, stderr)
	case "reserve", "export", "import", "status", "reconcile":
		value, err = ownerRecycleCustodyCommand(ctx, args, stderr)
	default:
		err = errors.New("unknown owner-recycle command; signing and broadcasting are not installed")
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// The explicit policy/owner predate the RPC response; observed pins are not
// automatically approved. Every storage read uses one authenticated header hash.
func ownerRecycleObserveCommand(ctx context.Context, args []string, stderr io.Writer) (ownerRecycleObservation, error) {
	var result ownerRecycleObservation
	flags := flag.NewFlagSet("owner-recycle observe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	policyPath := flags.String("policy", "", "independently reviewed recycle policy JSON")
	owner := flags.String("owner-account-id", "", "independently pinned raw AccountId32")
	rpcUrl := flags.String("rpc", "", "explicit owned HTTP(S) RPC")
	retry := flags.Duration("retry-window", 60*time.Second, "bounded read window")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *policyPath == "" || !rootCanonicalHash(*owner) || *rpcUrl == "" {
		return result, errors.New("recycle observe requires --policy FILE --owner-account-id HEX --rpc URL")
	}
	var policy recyclePolicy
	if _, err := readEconomicInput(*policyPath, &policy); err != nil {
		return result, err
	}
	if err := policy.validate(); err != nil {
		return result, err
	}
	client, err := newRpcClient(*rpcUrl, *retry)
	if err != nil {
		return result, err
	}
	defer client.httpClient.CloseIdleConnections()
	profile := rootReceiptProfile{RuntimeSourceCommit: policy.RuntimeSourceCommit, RuntimeVersion: policy.RuntimeVersion, RuntimeCodeHash: policy.RuntimeCodeHash, RuntimeMetadataHash: policy.RuntimeMetadataHash}
	native, err := newRootCanonicalChain(client, identityExpectation{NativeChain: policy.NativeChain, GenesisHash: policy.GenesisHash, EvmChainId: policy.EvmChainId}, []rootReceiptProfile{profile})
	if err != nil {
		return result, err
	}
	operationCtx, cancel := context.WithTimeout(ctx, *retry)
	defer cancel()
	if err := native.network(operationCtx); err != nil {
		return result, err
	}
	var hash string
	if err := client.call(operationCtx, "chain_getFinalizedHead", []any{}, &hash); err != nil {
		return result, err
	}
	_, number, err := native.header(operationCtx, hash)
	if err != nil {
		return result, err
	}
	result, err = native.recycleObservationAt(operationCtx, policy, *owner, hash, number)
	if err != nil {
		return ownerRecycleObservation{}, err
	}
	var canonical string
	if err := client.call(operationCtx, "chain_getBlockHash", []any{number}, &canonical); err != nil {
		return ownerRecycleObservation{}, err
	}
	if canonical != hash {
		return ownerRecycleObservation{}, errors.New("recycle finalized mapping changed during observation")
	}
	if err := native.network(operationCtx); err != nil {
		return ownerRecycleObservation{}, err
	}
	return result, nil
}

// Owner-side inspection opens only explicit local paths and has no host-custody
// or device effects. A Ledger transcript requires an independently pinned proof.
func ownerRecycleRequestCommand(ctx context.Context, args []string, stderr io.Writer) (any, error) {
	flags := flag.NewFlagSet("owner-recycle "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("request", "", "portable request file")
	trust := ownerSigningTrust{}
	flags.StringVar(&trust.RequestHash, "accept-request-hash", "", "independently pinned request hash")
	flags.StringVar(&trust.ApprovalKey, "approval-key", "", "independent recycle approval public key")
	flags.StringVar(&trust.Owner, "owner-account-id", "", "independently pinned owner AccountId32")
	flags.StringVar(&trust.Genesis, "expected-genesis", "", "independently pinned genesis")
	var proofPath, proofHash string
	if args[0] == "ledger-plan" {
		flags.StringVar(&proofPath, "metadata-proof", "", "shortened RFC78 proof file")
		flags.StringVar(&proofHash, "metadata-proof-sha256", "", "independently pinned proof file hash")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *path == "" {
		return nil, errors.New("recycle request command requires explicit request and independent trust pins")
	}
	raw, _, err := readBootstrapRootFile(ctx, *path, ownerSigningRequestLimit)
	if err != nil {
		return nil, err
	}
	var request ownerRecycleSigningRequest
	if err := decodePlanJson(raw, &request); err != nil {
		return nil, err
	}
	if err := request.validate(trust); err != nil {
		return nil, err
	}
	if args[0] == "inspect-request" {
		return request, nil
	}
	proof, actual, err := readBootstrapRootFile(ctx, proofPath, ownerLedgerPayloadLimit)
	if err != nil || !planSha256(proofHash) || actual != proofHash {
		return nil, errors.Join(errors.New("recycle shortened metadata proof differs from independent pin"), err)
	}
	return request.ledgerTranscript(trust, proof)
}

// Only explicit approved host-custody paths are opened here. The public API can
// retain an externally supplied signature, but never issue or transmit one.
func ownerRecycleCustodyCommand(ctx context.Context, args []string, stderr io.Writer) (value any, resultErr error) {
	mode := args[0]
	flags := flag.NewFlagSet("owner-recycle "+mode, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "independently approved recycle execution config")
	key := flags.String("approval-key", "", "independent approval public key")
	accepted := flags.String("accept-action-hash", "", "independently accepted action request hash")
	var metadataPath, ledgerPath, signaturePath, signatureHash, requestHash string
	var ledgerResponse bool
	if mode == "export" {
		flags.StringVar(&metadataPath, "metadata", "", "exact metadata14 hex file")
		flags.StringVar(&ledgerPath, "ledger-metadata", "", "exact metadata15 hex file for Ledger profile")
	}
	if mode == "import" {
		flags.StringVar(&signaturePath, "signature", "", "original public signature hex file")
		flags.StringVar(&signatureHash, "signature-file-sha256", "", "independently pinned signature file hash")
		flags.StringVar(&requestHash, "accept-request-hash", "", "original exported request hash")
		flags.BoolVar(&ledgerResponse, "ledger-response", false, "require exactly 00 plus64 Ed25519 bytes, excluding status words")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *configPath == "" || !rootCanonicalHash(*key) || !planSha256(*accepted) {
		return nil, errors.New("recycle custody requires --config FILE --approval-key HEX --accept-action-hash HASH")
	}
	raw, _, err := readBootstrapRootFile(ctx, *configPath, 128*1024)
	if err != nil {
		return nil, err
	}
	var config ownerRecycleConfig
	if err := decodePlanJson(raw, &config); err != nil {
		return nil, err
	}
	if err := config.validate(*key); err != nil {
		return nil, err
	}
	if config.Action.RequestHash != *accepted {
		return nil, errors.New("recycle action differs from independently accepted hash")
	}
	for _, path := range []string{*configPath, metadataPath, ledgerPath, signaturePath} {
		if path == config.Action.StatePath || path == config.Action.StatePath+".lock" {
			return nil, errors.New("recycle input overlaps custody journal")
		}
	}
	store, err := openOwnerRecycleStore(config, *key, mode == "reserve", ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr == nil {
			_, resultErr = store.load()
		}
		resultErr = errors.Join(resultErr, store.close())
		if resultErr != nil {
			value = nil
		}
	}()
	custody := ownerRecycleCustody{config: config, key: *key, store: store}
	switch mode {
	case "reserve", "status":
		return custody.load()
	case "export":
		metadata, _, err := readBootstrapRootFile(ctx, metadataPath, 2*maxMetadataRpcReplyBytes+3)
		if err != nil {
			return nil, err
		}
		var ledger []byte
		if ledgerPath != "" {
			ledger, _, err = readBootstrapRootFile(ctx, ledgerPath, 2*maxMetadataRpcReplyBytes+3)
			if err != nil {
				return nil, err
			}
		}
		return custody.export(strings.TrimSpace(string(metadata)), strings.TrimSpace(string(ledger)))
	case "import":
		encoded, actual, err := readBootstrapRootFile(ctx, signaturePath, 256)
		if err != nil || !planSha256(signatureHash) || actual != signatureHash {
			return nil, errors.Join(errors.New("recycle signature file differs from pinned hash"), err)
		}
		signature, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(encoded)), "0x"))
		if err != nil {
			return nil, err
		}
		if ledgerResponse {
			if config.Action.SignatureScheme != "ed25519" || len(signature) != 65 || signature[0] != 0 {
				return nil, errors.New("recycle Ledger response is not exactly MultiSignature::Ed25519")
			}
			signature = signature[1:]
		}
		return custody.importSignature(requestHash, signature)
	case "reconcile":
		chain, err := newOwnerRecycleCanonicalChain(config, *key)
		if err != nil {
			return nil, err
		}
		defer chain.client.httpClient.CloseIdleConnections()
		return custody.reconcile(ctx, chain)
	}
	return nil, errors.New("unknown recycle custody operation")
}
