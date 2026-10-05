// Opening stake comes from a read-only call to the exact original parent
// runtime. These proof-relative observations do not classify stock as income
// or confer independent runtime/layout/finality authority.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
)

const historicalPrincipalSchema = "urnetwork-original-parent-stake-api-v1"
const historicalPrincipalApi = "StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid"
const historicalPrincipalMaximum = 4096
const historicalPrincipalReportLimit = 4 * 1024 * 1024

// A canonical sorted census selects identities, never a caller-supplied amount.
type historicalPrincipalQuery struct {
	Hotkey  historicalReplayDigest `json:"hotkey"`
	Coldkey historicalReplayDigest `json:"coldkey"`
	Netuid  uint16                 `json:"netuid"`
}

// The detached SCALE result remains beside the derived scalar. Nil denotes an
// absent API result and differs from an explicitly observed zero stake.
type historicalPrincipalObservation struct {
	Query             historicalPrincipalQuery `json:"query"`
	ResultHex         string                   `json:"result_hex"`
	OpeningStakeAlpha *string                  `json:"opening_stake_alpha"`
	Registered        *bool                    `json:"registered"`
}

func validateHistoricalPrincipalQueries(queries []historicalPrincipalQuery) error {
	if queries == nil {
		return nil
	}
	if len(queries) == 0 || len(queries) > historicalPrincipalMaximum {
		return errors.New("historical principal census is empty or exceeds bound")
	}
	for index, query := range queries {
		if query.Netuid == 0 || query.Hotkey == (historicalReplayDigest{}) || query.Coldkey == (historicalReplayDigest{}) {
			return errors.New("historical principal census contains an invalid identity")
		}
		if index != 0 {
			previous := queries[index-1]
			order := bytes.Compare(previous.Hotkey[:], query.Hotkey[:])
			if order == 0 {
				order = bytes.Compare(previous.Coldkey[:], query.Coldkey[:])
				if order == 0 {
					order = int(previous.Netuid) - int(query.Netuid)
				}
			}
			if order >= 0 {
				return errors.New("historical principal census is unordered or repeated")
			}
		}
	}
	return nil
}

// Reparse the exact reviewed StakeInfo SCALE shape independently of the child
// report. The original runtime returns shares converted to stake; raw Alpha
// storage and a vault credit snapshot are not substitutes for this call.
func validateHistoricalPrincipalReport(queries []historicalPrincipalQuery, observations []historicalPrincipalObservation) error {
	if err := validateHistoricalPrincipalQueries(queries); err != nil {
		return err
	}
	if (queries == nil) != (observations == nil) || len(queries) != len(observations) {
		return errors.New("historical principal report substituted its exact query census")
	}
	for index, observation := range observations {
		query := queries[index]
		if observation.Query != query {
			return errors.New("historical principal report substituted requested identity")
		}
		raw, err := historicalReplayHex(observation.ResultHex, 256)
		if err != nil || len(raw) == 0 {
			return errors.Join(errors.New("historical principal result shape differs"), err)
		}
		if raw[0] == 0 {
			if len(raw) != 1 || observation.OpeningStakeAlpha != nil || observation.Registered != nil {
				return errors.New("historical principal absent result was relabelled known")
			}
			continue
		}
		if raw[0] != 1 || len(raw) < 65 || !bytes.Equal(raw[1:33], query.Hotkey[:]) || !bytes.Equal(raw[33:65], query.Coldkey[:]) {
			return errors.New("historical principal result substituted requested identity")
		}
		reader := rootScaleReader{data: raw, offset: 65}
		values := [6]uint64{}
		for item := range values {
			value, err := reader.compact()
			if err != nil {
				return fmt.Errorf("historical principal result field %d: %w", item, err)
			}
			values[item] = value
		}
		registered, err := reader.take(1)
		if err != nil || reader.offset != len(raw) || registered[0] > 1 || values[0] != uint64(query.Netuid) || observation.OpeningStakeAlpha == nil || *observation.OpeningStakeAlpha != strconv.FormatUint(values[1], 10) || observation.Registered == nil || *observation.Registered != (registered[0] == 1) {
			return errors.New("historical principal report differs from canonical original result")
		}
	}
	return nil
}
