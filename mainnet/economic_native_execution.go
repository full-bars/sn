// Native amounts are derived from the owned original-Wasm replay, not supplied
// in an approval. The original observation policy independently pins its reader
// authority; a new runtime or callsite layout cannot enroll itself through a job.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
)

const nativeExecutionAdmissionSchema = "urnetwork-native-miner-execution-admission-v1"
const nativeExecutionPolicySchema = "urnetwork-native-miner-execution-policy-v1"
const nativeExecutionAdmissionLimit = 1024 * 1024

type nativeExecutionPolicy struct {
	FeeCensus         *nativeFeeCensusPolicy         `json:"complete_fee_authority,omitempty"`
	Yuma              *nativeYumaPolicy              `json:"complete_allocation_authority,omitempty"`
	Principal         *nativePrincipalPolicy         `json:"opening_principal_authority,omitempty"`
	Schema            string                         `json:"schema"`
	ApprovalPublicKey string                         `json:"approval_ed25519_public_key"`
	ReviewSha256      string                         `json:"runtime_semantics_review_sha256"`
	ProfileSha256     string                         `json:"original_callsite_profile_sha256"`
	Engine            planFileReference              `json:"engine"`
	Directory         string                         `json:"admission_directory"`
	Producer          *nativeExecutionProducerPolicy `json:"producer,omitempty"`
}

func (self *nativeExecutionPolicy) validate() error {
	if self == nil {
		return nil
	}
	if self.Schema != nativeExecutionPolicySchema || !rootCanonicalHash(self.ApprovalPublicKey) || !planSha256(self.ReviewSha256) || !planSha256(self.ProfileSha256) || !bootstrapRootAbsolutePath(self.Engine.Path) || !planSha256(self.Engine.Sha256) || !bootstrapRootAbsolutePath(self.Directory) {
		return errors.New("native execution requires original independent runtime/layout/engine authority")
	}
	if self.FeeCensus != nil && self.Producer == nil {
		return errors.New("native complete fee authority requires the original continuous producer")
	}
	return errors.Join(self.Producer.validate(), self.Principal.validate(), self.Yuma.validate(), self.FeeCensus.validate())
}

// An admitted recipient is a registration generation, never merely an event UID.
// Unlisted ordinary miners are reported as residual entitlement, not providers.
type nativeExecutionRecipient struct {
	Uid        uint16 `json:"uid"`
	Hotkey     string `json:"hotkey"`
	Registered uint64 `json:"registered_at"`
	Coldkey    string `json:"coldkey,omitempty"`
}

type nativeExecutionAdmission struct {
	FeeCensus         *nativeFeeCensusPolicy     `json:"complete_fee_authority,omitempty"`
	Yuma              *nativeYumaPolicy          `json:"complete_allocation_authority,omitempty"`
	Principal         *nativePrincipalPolicy     `json:"opening_principal_authority,omitempty"`
	Schema            string                     `json:"schema"`
	Network           planNetwork                `json:"network"`
	Netuid            uint16                     `json:"netuid"`
	Registration      uint64                     `json:"subnet_registration_block"`
	Generation        uint64                     `json:"subnet_generation"`
	Parent            economicEmissionBoundary   `json:"parent"`
	Child             economicEmissionBoundary   `json:"child"`
	Runtime           rootReceiptProfile         `json:"execution_runtime"`
	ReviewSha256      string                     `json:"runtime_semantics_review_sha256"`
	ProfileSha256     string                     `json:"original_callsite_profile_sha256"`
	EngineSha256      string                     `json:"engine_sha256"`
	Job               planFileReference          `json:"job"`
	Providers         []nativeExecutionRecipient `json:"provider_generations"`
	FinalityAuthority string                     `json:"finality_authority"`
	Signature         string                     `json:"signature_ed25519"`
}

func (self nativeExecutionAdmission) signingBytes() ([]byte, error) {
	self.Signature = ""
	raw, err := json.Marshal(self)
	if err != nil || len(raw) > nativeProducerAuthorityMaximum(self.FeeCensus) {
		return nil, errors.Join(errors.New("native execution admission frame exceeds bound"), err)
	}
	return append([]byte(nativeExecutionAdmissionSchema+"\x00"), raw...), nil
}

func (self nativeExecutionAdmission) validate(policy economicEmissionPolicy, block economicEmissionBlock, runtime rootReceiptProfile) error {
	expected := policy.Execution
	if expected == nil {
		return errors.New("legacy observation cannot enroll execution authority")
	}
	if err := expected.validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(self.FeeCensus, expected.FeeCensus) {
		return errors.New("native execution changed original complete fee authority")
	}
	if !reflect.DeepEqual(self.Yuma, expected.Yuma) || !reflect.DeepEqual(self.Principal, expected.Principal) || self.Schema != nativeExecutionAdmissionSchema || self.Network != policy.Network || self.Netuid != policy.Netuid || self.Registration != *policy.SubnetRegistrationBlock || self.Generation != *policy.SubnetGeneration || self.Child != block.Boundary || self.Parent.Number+1 != self.Child.Number || self.Parent.Hash != block.Header.ParentHash || self.Runtime != runtime || self.Runtime.RuntimeSourceCommit != frontierMappingSourceCommit || self.ReviewSha256 != expected.ReviewSha256 || self.ProfileSha256 != expected.ProfileSha256 || self.EngineSha256 != expected.Engine.Sha256 || self.FinalityAuthority != "independently-reviewed-finalized-boundary" || !bootstrapRootAbsolutePath(self.Job.Path) || !planSha256(self.Job.Sha256) || len(self.Providers) > int(policy.MaximumUids) {
		return errors.New("native execution approval differs from original runtime, boundary or economic identity")
	}
	seen := map[string]bool{}
	for _, recipient := range self.Providers {
		if recipient.Uid >= policy.MaximumUids || !rootCanonicalHash(recipient.Hotkey) || !rootCanonicalHash(recipient.Coldkey) || recipient.Registered > block.Boundary.Number || seen[recipient.Hotkey] {
			return errors.New("native execution provider generation is invalid or duplicated")
		}
		seen[recipient.Hotkey] = true
	}
	message, messageErr := self.signingBytes()
	key, keyErr := rootReceiptHex(expected.ApprovalPublicKey, ed25519.PublicKeySize)
	signature, signatureErr := rootOfflineSignatureBytes(self.Signature)
	if messageErr != nil || keyErr != nil || signatureErr != nil || !ed25519.Verify(key, message, signature) {
		return errors.New("native execution independent approval signature is invalid")
	}
	return nil
}

// Each amount has one execution boundary and an exact trace/job/approval digest.
// FixedPointTolerance covers the observed final normalization and u64 casts;
// it does not excuse Yuma disagreement or grant an economic activation approval.
type nativeExecutionOutcome struct {
	FeeCensus              *nativeFeeCensusProjection          `json:"original_fee_census,omitempty"`
	CertifiedWindow        *nativeExecutionFinalityProjection  `json:"original_finality_window,omitempty"`
	Yuma                   *nativeYumaProjection               `json:"complete_allocation_witness,omitempty"`
	PrincipalEffects       *nativePrincipalExecutionProjection `json:"principal_execution_effects,omitempty"`
	OpeningPrincipals      *nativePrincipalProjection          `json:"opening_principals,omitempty"`
	RecipientEffects       *nativeExecutionEffectProjection    `json:"recipient_effects,omitempty"`
	Boundary               economicEmissionBoundary            `json:"boundary"`
	ProducerAuthorityHash  string                              `json:"producer_authority_hash,omitempty"`
	FinalityProofHash      string                              `json:"finality_proof_hash,omitempty"`
	AdmissionHash          string                              `json:"admission_hash"`
	JobHash                string                              `json:"job_hash"`
	TraceHash              string                              `json:"trace_hash"`
	MinerAllocation        string                              `json:"miner_allocation_alpha"`
	ProviderEntitlement    string                              `json:"provider_entitlement_alpha"`
	OwnerRecycled          string                              `json:"owner_recycled_alpha"`
	ResidualEntitlement    string                              `json:"residual_entitlement_alpha"`
	CollateralCapture      string                              `json:"reward_collateral_capture_alpha"`
	FixedPointDust         string                              `json:"fixed_point_dust_alpha"`
	FixedPointTolerance    string                              `json:"fixed_point_tolerance_alpha"`
	AllocationDifference   string                              `json:"allocation_difference_alpha"`
	RedirectedToValidators string                              `json:"redirected_to_validators_alpha"`
	Recipients             []nativeExecutionRecipient          `json:"execution_generations"`
	AmountsAuthenticated   bool                                `json:"amounts_authenticated"`
	ContentHash            string                              `json:"content_hash"`
}

type nativeExecutionDrain struct {
	Key      string
	Fallback []byte
}

// Existing completions bind the aggregate grammar and the original trace. The
// separately sealed projection can be derived again without rewriting that
// original completion or pretending an older result retained recipient amounts.
func (self nativeExecutionOutcome) hash() string {
	self.ContentHash = ""
	self.FeeCensus = nil
	self.CertifiedWindow = nil
	self.RecipientEffects = nil
	self.OpeningPrincipals = nil
	self.PrincipalEffects = nil
	self.Yuma = nil
	return rootObjectHash(self)
}

func nativeCapture(record historicalReplayObservation, label string, width int) ([]byte, error) {
	if record.Native == nil || record.Native.ExecutionPhaseHex == nil || *record.Native.ExecutionPhaseHex != "0x02" {
		return nil, errors.New("native economic capture lacks actual Initialization phase")
	}
	for _, capture := range record.Native.Memory {
		if capture.Name == label {
			raw, err := historicalReplayHex(capture.BytesHex, historicalNativeCaptureLimit)
			if err != nil || width >= 0 && len(raw) != width {
				return nil, errors.New("native economic capture has wrong output width: " + label)
			}
			return raw, nil
		}
	}
	return nil, errors.New("native economic capture omitted " + label)
}

func nativeCaptureUint(record historicalReplayObservation, label string, width int) (uint64, error) {
	raw, err := nativeCapture(record, label, width)
	if err != nil {
		return 0, err
	}
	switch width {
	case 1:
		return uint64(raw[0]), nil
	case 2:
		return uint64(binary.LittleEndian.Uint16(raw)), nil
	case 8:
		return binary.LittleEndian.Uint64(raw), nil
	}
	return 0, errors.New("native scalar width is unsupported")
}

func nativeCaptureVector(record historicalReplayObservation, label string, count int) ([]uint64, error) {
	raw, err := nativeCapture(record, label, count*8)
	if err != nil {
		return nil, err
	}
	values := make([]uint64, count)
	for index := range values {
		values[index] = binary.LittleEndian.Uint64(raw[index*8:])
	}
	return values, nil
}

// All decoded values come from the owned VM's captures. Approval carries no
// alpha amounts. Read absence, omitted captures or ambiguous order refuse.
func deriveNativeExecution(policy economicEmissionPolicy, admission nativeExecutionAdmission, block economicEmissionBlock, job historicalReplayJob, report historicalReplayReport, drainKeys [3]nativeExecutionDrain) (*nativeExecutionOutcome, error) {
	profile, trace := job.ObservationProfile, report.HookObservations
	if profile == nil || profile.Schema != historicalNativeProfileSchema || trace == nil || (profile.MetadataSha256 != nil) != (admission.FeeCensus != nil) {
		return nil, errors.New("native execution omits its dedicated original-memory profile")
	}
	if err := admission.FeeCensus.validate(); err != nil {
		return nil, err
	}
	if !report.PostStateReproduced || "sha256:"+hex.EncodeToString(report.JobSha256[:]) != admission.Job.Sha256 || report.ParentHash != job.ParentHash || report.ChildHash != job.ChildHash || report.RuntimeCodeSha256 != job.RuntimeCodeSha256 {
		return nil, errors.New("native execution report lacks the exact successful original replay")
	}
	if err := profile.validate(job); err != nil {
		return nil, err
	}
	if err := validateHistoricalReplayObservations(job, trace); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, err
	}
	if monitorReadDigest(raw) != admission.ProfileSha256 || "sha256:"+hex.EncodeToString(profile.SourceReviewSha256[:]) != admission.ReviewSha256 || "0x"+hex.EncodeToString(job.ParentHash[:]) != admission.Parent.Hash || "0x"+hex.EncodeToString(job.ChildHash[:]) != admission.Child.Hash || "0x"+hex.EncodeToString(job.RuntimeCodeBlake2b256[:]) != admission.Runtime.RuntimeCodeHash {
		return nil, errors.New("native execution job or raw memory profile differs from independent approval")
	}
	var drains []*historicalReplayObservation
	var epoch, emission *historicalReplayObservation
	recipients := []historicalReplayObservation{}
	for index := range trace.Observations {
		record := &trace.Observations[index]
		if nativeFeeCensusPurpose(record.Purpose) {
			if admission.FeeCensus == nil {
				return nil, errors.New("native execution cannot borrow unapproved complete fee paths")
			}
			continue
		}
		if historicalPrincipalEffectPurpose(record.Purpose) || historicalYumaPurpose(record.Purpose) {
			continue
		}
		// The same original runtime callsite can execute for several subnets.
		// Its actual netuid remains part of the authenticated memory capture.
		netuid, err := nativeCaptureUint(*record, "netuid", 2)
		if err != nil {
			return nil, err
		}
		if netuid != uint64(policy.Netuid) {
			continue
		}
		switch record.Purpose {
		case "native-drain":
			if len(drains) == len(drainKeys) || epoch != nil || record.Operation != "get" || record.KeyHex != drainKeys[len(drains)].Key || record.StorageReturn == nil {
				return nil, errors.New("native original drain is missing, repeated or out of order")
			}
			drains = append(drains, record)
		case "native-epoch":
			if len(drains) != len(drainKeys) || epoch != nil || emission != nil || len(recipients) != 0 {
				return nil, errors.New("native original normalization is ambiguous or out of order")
			}
			epoch = record
		case "native-emission":
			if epoch == nil || emission != nil || len(recipients) != 0 || record.Operation != "append" {
				return nil, errors.New("native original emission precedes or repeats normalization")
			}
			emission = record
		case "native-miner-credit", "native-owner-recycle":
			if emission == nil || record.Operation != "set" {
				return nil, errors.New("native recipient effect precedes original normalization")
			}
			recipients = append(recipients, *record)
		default:
			return nil, errors.New("native economic profile contains an unrelated semantic observation")
		}
	}
	result := &nativeExecutionOutcome{Boundary: block.Boundary, AdmissionHash: rootObjectHash(admission), JobHash: admission.Job.Sha256, TraceHash: rootObjectHash(trace), MinerAllocation: "0", ProviderEntitlement: "0", OwnerRecycled: "0", ResidualEntitlement: "0", CollateralCapture: "0", FixedPointDust: "0", FixedPointTolerance: "0", RedirectedToValidators: "0", AllocationDifference: "0", Recipients: []nativeExecutionRecipient{}}
	if len(drains) == 0 && epoch == nil && emission == nil && len(recipients) == 0 {
		for _, event := range block.Events {
			if event.Kind == "SubtensorModule.IncentiveAlphaEmittedToMiners" {
				return nil, errors.New("native replay omitted a runtime incentive execution")
			}
		}
		result.AmountsAuthenticated = true
		result.ContentHash = result.hash()
		result.RecipientEffects = newNativeExecutionEffects(admission.Parent, *result, nil, nil, nil)
		return result, nil
	}
	if len(drains) != len(drainKeys) || epoch == nil || emission == nil || len(block.Events) != 1 || block.Events[0].Kind != "SubtensorModule.IncentiveAlphaEmittedToMiners" {
		return nil, errors.New("native original drain, normalization or event is unavailable")
	}
	var tranches [3]uint64
	for index, record := range drains {
		// An explicit observed absence uses only the independently authenticated
		// metadata query default. A missing return record never means zero.
		raw := drainKeys[index].Fallback
		if record.StorageReturn.Present {
			if record.StorageReturn.ValueHex == nil {
				return nil, errors.New("native actual drain omitted returned bytes")
			}
			raw, err = historicalReplayHex(*record.StorageReturn.ValueHex, 8)
			if err != nil {
				return nil, err
			}
		}
		if len(raw) != 8 {
			return nil, errors.New("native actual drain or metadata default is not an exact u64")
		}
		tranches[index] = binary.LittleEndian.Uint64(raw)
	}
	miner, validator, root := tranches[0], tranches[1], tranches[2]
	for label, expected := range map[string]uint64{"subnet-registered": *policy.SubnetRegistrationBlock, "subnet-generation": *policy.SubnetGeneration} {
		value, err := nativeCaptureUint(*drains[0], label, 8)
		if err != nil || value != expected {
			return nil, errors.New("native drain lost its original subnet generation")
		}
	}
	count := len(block.Events[0].AlphaByUid)
	if count > int(policy.MaximumUids) {
		return nil, errors.New("native execution UID census exceeds original bound")
	}
	total, err := nativeCaptureUint(*epoch, "total-alpha", 8)
	if err != nil {
		return nil, err
	}
	trancheTotal := new(big.Int).Add(new(big.Int).SetUint64(miner), new(big.Int).SetUint64(validator))
	trancheTotal.Add(trancheTotal, new(big.Int).SetUint64(root))
	if trancheTotal.BitLen() > 64 {
		trancheTotal.SetUint64(^uint64(0))
	}
	if total != trancheTotal.Uint64() {
		return nil, errors.New("native normalization total includes an unobserved tranche or owner cut")
	}
	incentives, err := nativeCaptureVector(*epoch, "incentive-q32", count)
	if err != nil {
		return nil, err
	}
	dividends, err := nativeCaptureVector(*epoch, "dividends-q32", count)
	if err != nil {
		return nil, err
	}
	normalized, err := nativeCaptureVector(*epoch, "normalized-q32", count)
	if err != nil {
		return nil, err
	}
	emitted, err := nativeCaptureVector(*epoch, "emission", count)
	if err != nil {
		return nil, err
	}
	registered, err := nativeCaptureVector(*epoch, "registered", count)
	if err != nil {
		return nil, err
	}
	hotkeys, err := nativeCapture(*epoch, "hotkeys", count*32)
	if err != nil {
		return nil, err
	}
	uids, err := nativeCapture(*epoch, "uids", count*2)
	if err != nil {
		return nil, err
	}
	allocation, dust, tolerance, err := nativeExecutionArithmetic(total, incentives, dividends, normalized, emitted)
	if err != nil {
		return nil, err
	}
	result.MinerAllocation, result.FixedPointDust, result.FixedPointTolerance = fmt.Sprint(miner), dust, tolerance
	result.AllocationDifference = new(big.Int).Sub(new(big.Int).SetUint64(miner), allocation).String()
	if allocation.Sign() == 0 {
		result.RedirectedToValidators = result.MinerAllocation
	}
	byHotkey, used := map[string]nativeExecutionRecipient{}, map[string]bool{}
	for index := 0; index < count; index++ {
		if binary.LittleEndian.Uint16(uids[index*2:]) != uint16(index) || registered[index] > block.Boundary.Number || fmt.Sprint(emitted[index]) != block.Events[0].AlphaByUid[index] {
			return nil, errors.New("native original UID order, generation or emitted integer differs")
		}
		value := nativeExecutionRecipient{Uid: uint16(index), Hotkey: "0x" + hex.EncodeToString(hotkeys[index*32:(index+1)*32]), Registered: registered[index]}
		if _, exists := byHotkey[value.Hotkey]; exists {
			return nil, errors.New("native original hotkey occupies two UIDs")
		}
		byHotkey[value.Hotkey] = value
		result.Recipients = append(result.Recipients, value)
	}
	providers := map[string]nativeExecutionRecipient{}
	for _, provider := range admission.Providers {
		if actual, ok := byHotkey[provider.Hotkey]; !ok || actual.Uid != provider.Uid || actual.Registered != provider.Registered {
			return nil, errors.New("native provider registration generation was replaced")
		}
		providers[provider.Hotkey] = provider
	}
	providerTotal, ownerTotal, residualTotal, capturedTotal := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
	effects := make([]nativeExecutionEffect, 0, len(recipients))
	for _, record := range recipients {
		hotkey, err := nativeCapture(record, "hotkey", 32)
		if err != nil {
			return nil, err
		}
		identity, exists := byHotkey["0x"+hex.EncodeToString(hotkey)]
		if !exists || used[identity.Hotkey] {
			return nil, errors.New("native recipient effect changed or duplicated original generation")
		}
		uid := identity.Uid
		used[identity.Hotkey] = true
		coldkey, err := nativeCapture(record, "coldkey", 32)
		if err != nil {
			return nil, err
		}
		result.Recipients[uid].Coldkey = "0x" + hex.EncodeToString(coldkey)
		if provider, ok := providers[identity.Hotkey]; ok && provider.Coldkey != result.Recipients[uid].Coldkey {
			return nil, errors.New("native provider reward owner changed")
		}
		gross, err := nativeCaptureUint(record, "gross", 8)
		if err != nil {
			return nil, err
		}
		if gross != emitted[uid] {
			return nil, errors.New("native reward, capture and actual recycling do not conserve original emission")
		}
		effect := nativeExecutionEffect{Ordinal: record.Ordinal, Recipient: result.Recipients[uid], Branch: record.Purpose, Gross: fmt.Sprint(gross), Liquid: "0", Collateral: "0", Recycled: "0"}
		_, effect.Provider = providers[identity.Hotkey]
		if record.Purpose == "native-owner-recycle" {
			// The admitted original body/range identifies the owner Recycle
			// branch, excluding burn, root recycling and separate owner cut.
			recycled, err := nativeCaptureUint(record, "recycled", 8)
			if err != nil || recycled != gross {
				return nil, errors.New("native owner recycling differs from original entitlement")
			}
			if _, provider := providers[identity.Hotkey]; provider {
				return nil, errors.New("native provider is also a runtime owner recycle recipient")
			}
			ownerTotal.Add(ownerTotal, new(big.Int).SetUint64(recycled))
			effect.Recycled = fmt.Sprint(recycled)
		} else {
			captured, err := nativeCaptureUint(record, "captured", 8)
			if err != nil {
				return nil, err
			}
			liquid, err := nativeCaptureUint(record, "liquid", 8)
			if err != nil || captured > gross || liquid != gross-captured {
				return nil, errors.New("native collateral capture and liquid reward do not conserve original entitlement")
			}
			capturedTotal.Add(capturedTotal, new(big.Int).SetUint64(captured))
			effect.Liquid, effect.Collateral = fmt.Sprint(liquid), fmt.Sprint(captured)
			if _, provider := providers[identity.Hotkey]; provider {
				providerTotal.Add(providerTotal, new(big.Int).SetUint64(gross))
			} else {
				residualTotal.Add(residualTotal, new(big.Int).SetUint64(gross))
			}
		}
		effects = append(effects, effect)
	}
	for _, identity := range result.Recipients {
		if emitted[identity.Uid] != 0 && !used[identity.Hotkey] {
			return nil, errors.New("native execution omitted a nonzero original recipient effect")
		}
	}
	result.ProviderEntitlement, result.OwnerRecycled, result.ResidualEntitlement, result.CollateralCapture = providerTotal.String(), ownerTotal.String(), residualTotal.String(), capturedTotal.String()
	result.AmountsAuthenticated = true
	result.ContentHash = result.hash()
	eventIndex, emissionOrdinal := block.Events[0].EventIndex, emission.Ordinal
	result.RecipientEffects = newNativeExecutionEffects(admission.Parent, *result, &eventIndex, &emissionOrdinal, effects)
	if err := result.RecipientEffects.validate(*result); err != nil {
		return nil, err
	}
	return result, nil
}

// Signed I32F32 normalization is reproduced with arbitrary-width intermediates;
// multiplying I96F32 by an integer then casting u64 is an exact right shift.
// No float, saturated host conversion or guessed percentage tolerance is used.
func nativeExecutionArithmetic(total uint64, incentive, dividend, normalized, emitted []uint64) (*big.Int, string, string, error) {
	count := len(incentive)
	if count > rootCensusLimit || len(dividend) != count || len(normalized) != count || len(emitted) != count {
		return nil, "", "", errors.New("native arithmetic input dimensions differ")
	}
	sum := new(big.Int)
	for index := range incentive {
		if incentive[index] > 1<<32 || dividend[index] > 1<<32 {
			return nil, "", "", errors.New("native nonnegative I32F32 input exceeds normalized domain")
		}
		sum.Add(sum, new(big.Int).SetUint64(incentive[index]+dividend[index]))
	}
	allocation, ideal := new(big.Int), new(big.Rat)
	for index := range incentive {
		bits := new(big.Int)
		if sum.Sign() != 0 {
			bits.Quo(new(big.Int).Lsh(new(big.Int).SetUint64(incentive[index]), 32), sum)
			ideal.Add(ideal, new(big.Rat).SetFrac(new(big.Int).Mul(new(big.Int).SetUint64(total), new(big.Int).SetUint64(incentive[index])), sum))
		}
		if bits.Uint64() != normalized[index] {
			return nil, "", "", errors.New("native captured I32F32 normalization differs")
		}
		amount := new(big.Int).Rsh(new(big.Int).Mul(new(big.Int).SetUint64(total), bits), 32)
		if !amount.IsUint64() || amount.Uint64() != emitted[index] {
			return nil, "", "", errors.New("native I96F32 multiplication or per-UID u64 cast differs")
		}
		allocation.Add(allocation, amount)
	}
	loss := new(big.Rat).Sub(ideal, new(big.Rat).SetInt(allocation))
	if loss.Sign() < 0 {
		return nil, "", "", errors.New("native quantization created allocation")
	}
	dust, remainder := new(big.Int).QuoRem(loss.Num(), loss.Denom(), new(big.Int))
	tolerance := new(big.Int).Set(dust)
	if remainder.Sign() != 0 {
		tolerance.Add(tolerance, big.NewInt(1))
	}
	return allocation, dust.String(), tolerance.String(), nil
}

func observeNativeExecution(ctx context.Context, client *rpcClient, policy economicEmissionPolicy, block economicEmissionBlock, runtime rootReceiptProfile, metadata *types.Metadata) (*nativeExecutionOutcome, error) {
	if policy.Execution == nil {
		return nil, nil
	}
	if policy.Execution.Producer != nil {
		session, ok := ctx.Value(nativeProducerSessionKey{}).(*nativeProducerSession)
		if !ok || session == nil {
			return nil, errors.New("native execution producer session is absent")
		}
		return session.observe(ctx, client, block, runtime, metadata)
	}
	path := filepath.Join(policy.Execution.Directory, strings.TrimPrefix(block.Boundary.Hash, "0x")+".json")
	raw, _, err := readPlanFile(ctx, path, nativeExecutionAdmissionLimit)
	if err != nil {
		return nil, err
	}
	var admission nativeExecutionAdmission
	if err := decodePlanJson(raw, &admission); err != nil {
		return nil, err
	}
	if err := admission.validate(policy, block, runtime); err != nil {
		return nil, err
	}
	jobRaw, digest, err := readPlanFile(ctx, admission.Job.Path, historicalReplayJobLimit)
	if err != nil {
		return nil, err
	}
	if digest != admission.Job.Sha256 {
		return nil, errors.Join(errRpcIntegrity, errors.New("native original replay job bytes differ"))
	}
	var job historicalReplayJob
	if err := decodePlanJson(jobRaw, &job); err != nil {
		return nil, err
	}
	profileRaw, err := json.Marshal(job.ObservationProfile)
	if err != nil {
		return nil, err
	}
	code, err := historicalReplayHex(job.RuntimeCodeHex, 8*1024*1024)
	if err != nil {
		return nil, err
	}
	if job.PrincipalEffects != policy.Execution.Principal.effectsAt(admission.Parent) || !reflect.DeepEqual(job.PrincipalQueries, policy.Execution.Principal.queriesAt(admission.Parent)) || job.ObservationProfile == nil || monitorReadDigest(profileRaw) != admission.ProfileSha256 || job.RuntimeCodeSha256 != historicalReplayDigest(sha256.Sum256(code)) || len(job.ExtrinsicsHex) != block.BodyCount {
		return nil, errors.New("native replay input differs before process admission")
	}
	report, err := runHistoricalReplay(ctx, historicalReplayRequest{Engine: policy.Execution.Engine, Job: admission.Job, Budget: client.retryWindow}, historicalReplayHooks{})
	if err != nil {
		return nil, err
	}
	return validateNativeExecutionReplay(ctx, policy, admission, block, metadata, job, report)
}

// Both the legacy signed fixture path and the continuous proof producer use
// the exact event/layout/amount verifier after their independent admission.
func validateNativeExecutionReplay(ctx context.Context, policy economicEmissionPolicy, admission nativeExecutionAdmission, block economicEmissionBlock, metadata *types.Metadata, job historicalReplayJob, report *historicalReplayReport) (*nativeExecutionOutcome, error) {
	if report == nil || len(job.ExtrinsicsHex) != block.BodyCount {
		return nil, errors.Join(errRpcIntegrity, errors.New("native execution report or original body count differs"))
	}
	if report.HookObservations != nil {
		for _, record := range report.HookObservations.Observations {
			if record.Purpose != "native-emission" {
				continue
			}
			netuid, err := nativeCaptureUint(record, "netuid", 2)
			if err != nil {
				return nil, errors.Join(errRpcIntegrity, err)
			}
			eventKey, err := types.CreateStorageKey(metadata, "System", "Events")
			if err != nil {
				return nil, err
			}
			if record.Operation != "append" || record.KeyHex != eventKey.Hex() || record.ValueHex == nil {
				return nil, errors.Join(errRpcIntegrity, errors.New("native epoch lacks original System.Events append"))
			}
			eventRaw, err := historicalReplayHex(*record.ValueHex, economicEmissionEventBytesLimit-1)
			if err != nil {
				return nil, err
			}
			events, contexts, err := decodeEconomicEmissionEvents(metadata, append([]byte{4}, eventRaw...), block.BodyCount, uint16(netuid), rootCensusLimit, block.Boundary.Number)
			if err != nil || len(events) != 1 || len(contexts) != 0 {
				return nil, errors.Join(errRpcIntegrity, errors.New("native original event cannot be decoded under admitted metadata"), err)
			}
			if netuid != uint64(policy.Netuid) {
				continue
			}
			if len(block.Events) != 1 {
				return nil, errors.Join(errRpcIntegrity, errors.New("native original event differs from target event census"))
			}
			events[0].EventIndex = block.Events[0].EventIndex
			if !reflect.DeepEqual(events[0], block.Events[0]) {
				return nil, errors.Join(errRpcIntegrity, errors.New("native original event contradicts observed block"))
			}
		}
	}
	entries, err := observationStorageProfile(metadata, economicEmissionStorageSpecs)
	if err != nil {
		return nil, err
	}
	var drains [3]nativeExecutionDrain
	for index, name := range []string{"PendingServerEmission", "PendingValidatorEmission", "PendingRootAlphaDivs"} {
		key, err := types.CreateStorageKey(metadata, "SubtensorModule", name, binary.LittleEndian.AppendUint16(nil, policy.Netuid))
		if err != nil {
			return nil, err
		}
		drains[index] = nativeExecutionDrain{Key: key.Hex(), Fallback: append([]byte(nil), entries[name].Fallback...)}
	}
	result, err := deriveNativeExecution(policy, admission, block, job, *report, drains)
	if err != nil {
		return nil, nativeExecutionDerivationError(err)
	}
	result.Yuma, err = deriveNativeYuma(ctx, policy, admission, report, *result)
	if err != nil {
		return nil, nativeExecutionDerivationError(err)
	}
	result.PrincipalEffects, err = deriveNativePrincipalEffects(policy, admission, job, report, *result)
	if err != nil {
		return nil, nativeExecutionDerivationError(err)
	}
	result.OpeningPrincipals, err = deriveNativePrincipal(policy, admission, job, report, *result)
	if err != nil {
		return nil, nativeExecutionDerivationError(err)
	}
	return result, nil
}
