package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Artifact provenance is the operator's retained public artifact role. Root
// transaction authority comes separately from the historical operator version
// and the exact committed artifact hash, Merkle root and receipt.
func finalOperatorArtifactSigners(identities *finalPublicIdentities, deploymentID string, operatorCount int) (map[uint64]common.Address, error) {
	if identities == nil || identities.Schema != "urnetwork-sim-public-identities-v1" || identities.DeploymentID != deploymentID || deploymentID == "" || operatorCount < 1 {
		return nil, errors.New("operator artifact identity source is incomplete")
	}
	result := make(map[uint64]common.Address, operatorCount)
	seen := map[common.Address]bool{}
	for noID := 1; noID <= operatorCount; noID++ {
		label := fmt.Sprintf("operator-%d-artifact", noID)
		value, found := identities.EVM[label]
		address := common.HexToAddress(value)
		if !found || !common.IsHexAddress(value) || address == (common.Address{}) || seen[address] {
			return nil, fmt.Errorf("operator artifact role %s is absent, invalid or reused", label)
		}
		seen[address] = true
		result[uint64(noID)] = address
	}
	return result, nil
}

// The signed validator audit names its actual artifact signer. Compare that
// projection to independently captured role bytes, never to the key that sent
// the coordinator transaction or to a signer recovered from the artifact itself.
func verifyFinalArtifactSignerProvenance(evidence *FinalSemanticEvidence, signers map[uint64]common.Address) error {
	if evidence == nil || len(signers) != evidence.ExpectedOperators {
		return errors.New("final artifact provenance has an incomplete operator census")
	}
	check := func(cycle *FinalCRv4Cycle) error {
		for _, pool := range cycle.Pools {
			expected, exists := signers[pool.NoID]
			if !exists || expected == (common.Address{}) || !strings.EqualFold(pool.ArtifactSigner, expected.Hex()) {
				return fmt.Errorf("operator %d signed artifact provenance differs from its retained artifact identity", pool.NoID)
			}
		}
		return nil
	}
	for _, validator := range evidence.Validators {
		for index := range validator.Cycles {
			if err := check(&validator.Cycles[index]); err != nil {
				return err
			}
		}
	}
	if evidence.DishonestDeposit != nil {
		for _, decisions := range [][]FinalDishonestDepositDecision{evidence.DishonestDeposit.Penalties, evidence.DishonestDeposit.Recoveries} {
			for index := range decisions {
				if err := check(&decisions[index].Cycle); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
