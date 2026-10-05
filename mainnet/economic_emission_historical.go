// Archive catchup authenticates each bounded original range without walking
// every intervening block to today's head. Historical finality is explicitly
// the configured RPC's assertion, never an independent finality proof.
package main

import (
	"context"
	"errors"
)

func economicEmissionFinalizedAssertion(ctx context.Context, chain *rootCanonicalChain) (economicEmissionBoundary, *rootReceiptHeader, error) {
	var hash string
	if err := chain.client.call(ctx, "chain_getFinalizedHead", []any{}, &hash); err != nil {
		return economicEmissionBoundary{}, nil, err
	}
	header, number, err := chain.header(ctx, hash)
	if err != nil {
		return economicEmissionBoundary{}, nil, err
	}
	return economicEmissionBoundary{Number: number, Hash: hash}, &header, nil
}

func economicEmissionHistoricalPage(ctx context.Context, chain *rootCanonicalChain, policy economicEmissionPolicy, budget *economicEmissionBudget, retain *[]rootReceiptHeader) (economicEmissionBoundary, *rootReceiptHeader, map[uint64]economicEmissionBlock, error) {
	head, finalizedHeader, err := economicEmissionFinalizedAssertion(ctx, chain)
	if err != nil {
		return head, finalizedHeader, nil, err
	}
	if head.Number < policy.Through.Number {
		return head, finalizedHeader, nil, errors.New("native economic archive page is ahead of observed finalized head")
	}
	if err := budget.retain(finalizedHeader); err != nil {
		return head, finalizedHeader, nil, err
	}
	blocks := map[uint64]economicEmissionBlock{}
	hash := policy.Through.Hash
	for number := policy.Through.Number; ; number-- {
		header, actual, err := chain.header(ctx, hash)
		if err != nil {
			return head, finalizedHeader, nil, err
		}
		if actual != number {
			return head, finalizedHeader, nil, errors.Join(errRpcIntegrity, errors.New("native economic archive page height is discontinuous"))
		}
		if err := budget.retain(header); err != nil {
			return head, finalizedHeader, nil, err
		}
		*retain = append(*retain, header)
		boundary := economicEmissionBoundary{Number: number, Hash: hash}
		blocks[number] = economicEmissionBlock{Boundary: boundary, Header: header}
		if number == policy.From.Number {
			if boundary != policy.From {
				return head, finalizedHeader, nil, errors.Join(errRpcIntegrity, errors.New("native economic archive page lost its original cursor"))
			}
			return head, finalizedHeader, blocks, nil
		}
		hash = header.ParentHash
	}
}
