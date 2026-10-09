package provider

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/hotkeywallet"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

const hotkeyWalletDirectoryName = "hotkey-wallet"

// An earning interval holds at most 65536 epochs.
const hotkeyWalletIntervalEpochs = 65535

// The wallet list may name every provider of the network.
const hotkeyWalletAnswerBytes = 16 * 1024 * 1024

// Keeps every computed interval end within uint64.
const hotkeyWalletMaximumEpoch = math.MaxUint64 - 4*(hotkeyWalletIntervalEpochs+1)

func hotkeyWalletStore(base string) hotkeywallet.Store {
	return hotkeywallet.Store{Directory: filepath.Join(base, hotkeyWalletDirectoryName)}
}

// One authenticated operator target.
type hotkeyWalletTarget struct {
	domain string
	apiUrl string
	byJwt  string
}

func hotkeyWalletSingleTarget(apiUrl, byJwt string) hotkeyWalletTarget {
	domain := apiUrl
	if u, err := url.Parse(apiUrl); err == nil && u.Host != "" {
		domain = u.Host
	}
	return hotkeyWalletTarget{domain: domain, apiUrl: apiUrl, byJwt: byJwt}
}

func hotkeyWalletTargetFromOpts(opts docopt.Opts) (hotkeyWalletTarget, error) {
	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		return hotkeyWalletTarget{}, err
	}
	byJwt, err := readNetworkJwt()
	if err != nil {
		return hotkeyWalletTarget{}, err
	}
	return hotkeyWalletSingleTarget(apiUrl, byJwt), nil
}

// One request the way hotkeywallet calls an operator: the network jwt as
// bearer, no redirects, a bounded JSON answer, and any status but 200 an
// error.
func hotkeyWalletCall(ctx context.Context, target hotkeyWalletTarget, method string, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	// the network jwt must never leave the box in cleartext
	origin, err := hotkeywallet.SecureApiUrl(target.apiUrl)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, origin+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+target.byJwt)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, hotkeyWalletAnswerBytes+1))
	if err != nil {
		return err
	}
	if len(answer) > hotkeyWalletAnswerBytes {
		return fmt.Errorf("%s %s answer exceeds %d bytes", method, path, hotkeyWalletAnswerBytes)
	}
	if response.StatusCode != http.StatusOK {
		// Bound server error bodies to 200 bytes per secret hygiene rules
		errLen := min(len(answer), 200)
		return fmt.Errorf("%s %s: %s: %s", method, path, response.Status, strings.TrimSpace(string(answer[:errLen])))
	}
	if result != nil {
		if err := json.Unmarshal(answer, result); err != nil {
			return fmt.Errorf("%s %s answer is not JSON: %w", method, path, err)
		}
	}
	return nil
}

// An operator's GET /sn/epoch: its current epoch and the subnet it states.
type hotkeyWalletEpochResult struct {
	Epoch       uint64 `json:"epoch"`
	ChainId     uint64 `json:"chain_id"`
	GenesisHash string `json:"genesis_hash"`
	Netuid      uint64 `json:"netuid"`
}

// The operator's answer, refused when its current epoch is implausible.
func hotkeyWalletEpoch(ctx context.Context, target hotkeyWalletTarget) (*hotkeyWalletEpochResult, error) {
	var result hotkeyWalletEpochResult
	if err := hotkeyWalletCall(ctx, target, http.MethodGet, "/sn/epoch", nil, &result); err != nil {
		return nil, err
	}
	if result.Epoch > hotkeyWalletMaximumEpoch {
		return nil, fmt.Errorf("the operator's current epoch %d is implausible", result.Epoch)
	}
	return &result, nil
}

// A network-level entry of GET /sn/wallet.
type hotkeyWalletEntry struct {
	ClientId          *string `json:"client_id"`
	ConsentScope      string  `json:"consent_scope"`
	ColdkeySs58       string  `json:"coldkey_ss58"`
	HotkeySs58        string  `json:"hotkey_ss58"`
	FromEpoch         uint64  `json:"from_epoch"`
	ThroughEpoch      uint64  `json:"through_epoch"`
	ConsentHeadHash   string  `json:"consent_head_hash"`
	ConsentGeneration uint64  `json:"consent_generation"`
}

// The network consent entry and the hotkey delegation entry, when listed.
func hotkeyWalletEntries(ctx context.Context, target hotkeyWalletTarget) (network *hotkeyWalletEntry, hotkey *hotkeyWalletEntry, returnErr error) {
	var result struct {
		Wallets []hotkeyWalletEntry `json:"wallets"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := hotkeyWalletCall(ctx, target, http.MethodGet, "/sn/wallet", nil, &result); err != nil {
		return nil, nil, err
	}
	if result.Error != nil {
		return nil, nil, fmt.Errorf("GET /sn/wallet: %s", result.Error.Message)
	}
	for index := range result.Wallets {
		entry := &result.Wallets[index]
		if entry.ClientId != nil || entry.FromEpoch > hotkeyWalletMaximumEpoch {
			continue
		}
		switch {
		case entry.ConsentScope == "network" && network == nil:
			network = entry
		case entry.ConsentScope == protocol.EarningWalletModeHotkey && hotkey == nil:
			hotkey = entry
		}
	}
	return network, hotkey, nil
}

// Whether the entry delegates the network to this head of the hotkey's chain.
func (self *hotkeyWalletEntry) adopts(hotkey [32]byte, headHash [32]byte, generation uint64) bool {
	if self == nil {
		return false
	}
	if self.ConsentScope != "" && self.ConsentScope != protocol.EarningWalletModeHotkey {
		return false
	}
	entryHotkey, err := ss58.DecodeWithPrefix(self.HotkeySs58, ss58.BittensorPrefix)
	if err != nil || entryHotkey != hotkey || self.ConsentGeneration != generation {
		return false
	}
	head, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(self.ConsentHeadHash, "0x"), "0X"))
	return err == nil && bytes.Equal(head, headHash[:])
}

// The subnet one operator states in GET /sn/epoch.
func hotkeyWalletOperatorSubnet(ctx context.Context, target hotkeyWalletTarget, coldkeySs58 string) (protocol.HotkeyWalletMappingSubnet, error) {
	epochResult, err := hotkeyWalletEpoch(ctx, target)
	if err != nil {
		return protocol.HotkeyWalletMappingSubnet{}, err
	}
	if epochResult.GenesisHash != "" {
		genesisHashHex, prefixed := strings.CutPrefix(epochResult.GenesisHash, "0x")
		genesisHash, err := hex.DecodeString(genesisHashHex)
		if !prefixed || err != nil || len(genesisHash) != 32 {
			return protocol.HotkeyWalletMappingSubnet{}, errors.New("GET /sn/epoch states a genesis_hash that is not 0x and 64 hex digits")
		}
		if epochResult.Netuid > math.MaxUint16 {
			return protocol.HotkeyWalletMappingSubnet{}, fmt.Errorf("GET /sn/epoch states netuid %d, beyond any subnet", epochResult.Netuid)
		}
		subnet := protocol.HotkeyWalletMappingSubnet{ChainID: epochResult.ChainId, GenesisHash: [32]byte(genesisHash), Netuid: uint16(epochResult.Netuid)}
		if err := subnet.Validate(); err != nil {
			return protocol.HotkeyWalletMappingSubnet{}, fmt.Errorf("GET /sn/epoch states chain %d genesis 0x%x netuid %d: %w", subnet.ChainID, subnet.GenesisHash, subnet.Netuid, err)
		}
		return subnet, nil
	}
	network, _, err := hotkeyWalletEntries(ctx, target)
	if err != nil {
		return protocol.HotkeyWalletMappingSubnet{}, err
	}
	from := epochResult.Epoch + 2
	if network != nil && from <= network.FromEpoch {
		from = network.FromEpoch + 1
	}
	var challenge struct {
		Message string `json:"message"`
	}
	args := map[string]any{"coldkey_ss58": coldkeySs58, "from_epoch": from, "through_epoch": from + hotkeyWalletIntervalEpochs}
	if err := hotkeyWalletCall(ctx, target, http.MethodPost, "/sn/wallet/network-consent", args, &challenge); err != nil {
		return protocol.HotkeyWalletMappingSubnet{}, err
	}
	statement, err := protocol.DecodeNetworkWalletMappingStatement(challenge.Message)
	if err != nil {
		return protocol.HotkeyWalletMappingSubnet{}, fmt.Errorf("the network consent challenge does not decode: %w", err)
	}
	subnet := statement.Domain.HotkeySubnet()
	return subnet, subnet.Validate()
}

// The subnet the reachable authenticated operators state.
func hotkeyWalletSubnet(ctx context.Context, targets []hotkeyWalletTarget, coldkeySs58 string, out io.Writer) (protocol.HotkeyWalletMappingSubnet, error) {
	var subnet protocol.HotkeyWalletMappingSubnet
	source := ""
	for _, target := range targets {
		stated, err := hotkeyWalletOperatorSubnet(ctx, target, coldkeySs58)
		if err != nil {
			fmt.Fprintf(out, "operator %s: its subnet is unavailable: %v\n", target.domain, err)
			continue
		}
		if source == "" {
			subnet, source = stated, target.domain
		} else if stated != subnet {
			return protocol.HotkeyWalletMappingSubnet{}, fmt.Errorf("operators disagree on the subnet: %s states chain %d genesis 0x%x netuid %d, %s states chain %d genesis 0x%x netuid %d", source, subnet.ChainID, subnet.GenesisHash, subnet.Netuid, target.domain, stated.ChainID, stated.GenesisHash, stated.Netuid)
		}
	}
	if source == "" {
		return protocol.HotkeyWalletMappingSubnet{}, errors.New("no authenticated operator stated its subnet")
	}
	return subnet, nil
}

// The latest current epoch among the reachable operators.
func hotkeyWalletCurrentEpoch(ctx context.Context, targets []hotkeyWalletTarget, out io.Writer) (uint64, error) {
	current, known := uint64(0), false
	for _, target := range targets {
		epochResult, err := hotkeyWalletEpoch(ctx, target)
		if err != nil {
			fmt.Fprintf(out, "operator %s: its current epoch is unavailable: %v\n", target.domain, err)
			continue
		}
		current, known = max(current, epochResult.Epoch), true
	}
	if !known {
		return 0, errors.New("no authenticated operator stated its current epoch; give --wallet-from-epoch and --wallet-through-epoch")
	}
	return current, nil
}

// The global consent's earning epochs from the command line.
type hotkeyWalletEpochs struct {
	from    uint64
	through uint64
}

func hotkeyWalletEpochsFromOpts(opts docopt.Opts) (*hotkeyWalletEpochs, error) {
	from, _ := opts.String("--wallet-from-epoch")
	through, _ := opts.String("--wallet-through-epoch")
	if (from == "") != (through == "") {
		return nil, errors.New("--wallet-from-epoch and --wallet-through-epoch go together")
	}
	if from == "" {
		return nil, nil
	}
	first, err := strconv.ParseUint(from, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("--wallet-from-epoch=%s is not an epoch", from)
	}
	last, err := strconv.ParseUint(through, 10, 64)
	if err != nil || last < first || last-first > hotkeyWalletIntervalEpochs {
		return nil, fmt.Errorf("--wallet-through-epoch=%s must be at most %d epochs after --wallet-from-epoch", through, hotkeyWalletIntervalEpochs)
	}
	return &hotkeyWalletEpochs{from: first, through: last}, nil
}

// The next generation's statement for the coldkey.
func hotkeyWalletNextStatement(ctx context.Context, store hotkeywallet.Store, targets []hotkeyWalletTarget, hotkey *crv4.Keypair, coldkey [32]byte, coldkeySs58 string, epochs *hotkeyWalletEpochs, out io.Writer) (*protocol.HotkeyWalletMappingStatement, error) {
	chain, err := store.Chain()
	if err != nil {
		return nil, err
	}
	var head *protocol.HotkeyWalletMappingStatement
	var subnet protocol.HotkeyWalletMappingSubnet
	if len(chain) == 0 {
		if subnet, err = hotkeyWalletSubnet(ctx, targets, coldkeySs58, out); err != nil {
			return nil, err
		}
	} else {
		if head, _, err = protocol.VerifyHotkeyWalletMappingLineage(ctx, chain); err != nil {
			return nil, err
		}
		subnet = head.Subnet
	}
	if epochs == nil {
		epochs = &hotkeyWalletEpochs{}
		if head != nil {
			current, err := hotkeyWalletCurrentEpoch(ctx, targets, out)
			if err != nil {
				return nil, err
			}
			epochs.from = max(current+1, head.FromEpoch+1)
		}
		epochs.through = epochs.from + hotkeyWalletIntervalEpochs
	}
	return hotkeywallet.NextStatement(chain, subnet, hotkey.PublicKey(), coldkey, epochs.from, epochs.through, time.Now())
}

// What ensureOperatorHotkeyWallet left at one operator.
type hotkeyWalletOutcome struct {
	generation   uint64
	delegated    bool
	fromEpoch    uint64
	throughEpoch uint64
}

func (self hotkeyWalletOutcome) String() string {
	if !self.delegated {
		return fmt.Sprintf("chain stored; the delegation already adopts generation %d", self.generation)
	}
	return fmt.Sprintf("chain stored; delegated to generation %d from epoch %d through %d", self.generation, self.fromEpoch, self.throughEpoch)
}

// refuseHotkeyTakeover stops a box from replacing the network's delegation to a
// different hotkey. The delegation is network-scoped and a fleet is one network,
// so two boxes with different hotkeys would take it back from each other every
// hour, each time moving the paid coldkey. Replacing another hotkey's delegation
// takes the explicit --replace-other-hotkey; the hourly upkeep never has it.
func refuseHotkeyTakeover(entry *hotkeyWalletEntry, hotkey [32]byte, replaceOther bool) error {
	if entry == nil || replaceOther {
		return nil
	}
	if entry.ConsentScope != "" && entry.ConsentScope != protocol.EarningWalletModeHotkey {
		return nil
	}
	entryHotkey, err := ss58.DecodeWithPrefix(entry.HotkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return fmt.Errorf("the operator lists a delegation whose hotkey %q cannot be read, so it is not replaced; check it with `provider wallet hotkey status`, or pass --replace-other-hotkey", entry.HotkeySs58)
	}
	if entryHotkey == hotkey {
		return nil
	}
	return fmt.Errorf("the network is already delegated to another hotkey (%s); replacing it moves the paid coldkey for every box of this network. Check `provider wallet hotkey status`, and pass --replace-other-hotkey only if you mean to take it over", entry.HotkeySs58)
}

// Stores the chain at one operator, then delegates the miner's network there
// to the chain's head unless the delegation already adopts it.
func ensureOperatorHotkeyWallet(ctx context.Context, target hotkeyWalletTarget, hotkey *crv4.Keypair, chain []protocol.HotkeyWalletMappingConsent, replaceOther bool) (hotkeyWalletOutcome, error) {
	operator := hotkeywallet.Operator{ApiUrl: target.apiUrl, ByJwt: target.byJwt}
	headHash, generation, err := operator.SubmitChain(ctx, chain)
	if err != nil {
		return hotkeyWalletOutcome{}, fmt.Errorf("storing the chain: %w", err)
	}
	outcome := hotkeyWalletOutcome{generation: generation}
	_, entry, err := hotkeyWalletEntries(ctx, target)
	if err != nil {
		return outcome, err
	}
	epochResult, err := hotkeyWalletEpoch(ctx, target)
	if err != nil {
		return outcome, err
	}
	// adopting the head is not enough: a delegation whose window has passed
	// earns nothing, so it is renewed like a missing one
	if entry.adopts(hotkey.PublicKey(), headHash, generation) && epochResult.Epoch+2 <= entry.ThroughEpoch {
		return outcome, nil
	}
	if err := refuseHotkeyTakeover(entry, hotkey.PublicKey(), replaceOther); err != nil {
		return outcome, err
	}
	outcome.delegated = true
	outcome.fromEpoch = epochResult.Epoch + 2
	if entry != nil && outcome.fromEpoch <= entry.FromEpoch {
		outcome.fromEpoch = entry.FromEpoch + 1
	}
	outcome.throughEpoch = outcome.fromEpoch + hotkeyWalletIntervalEpochs
	if err := operator.EnsureDelegation(ctx, hotkey, headHash, generation, outcome.fromEpoch, outcome.throughEpoch); err != nil {
		return outcome, fmt.Errorf("delegating the network: %w", err)
	}
	return outcome, nil
}

// Every authenticated operator in turn, each reported; returns how many failed.
func hotkeyWalletSubmit(ctx context.Context, targets []hotkeyWalletTarget, awaiting []string, hotkey *crv4.Keypair, chain []protocol.HotkeyWalletMappingConsent, replaceOther bool, out io.Writer) int {
	failed := 0
	for _, target := range targets {
		outcome, err := ensureOperatorHotkeyWallet(ctx, target, hotkey, chain, replaceOther)
		if err != nil {
			failed++
			fmt.Fprintf(out, "operator %s: failed: %v\n", target.domain, err)
			continue
		}
		fmt.Fprintf(out, "operator %s: %s\n", target.domain, outcome)
	}
	for _, domain := range awaiting {
		fmt.Fprintf(out, "operator %s: skipped, awaiting auth\n", domain)
	}
	return failed
}

func hotkeyWalletColdkey(opts docopt.Opts) (string, [32]byte, error) {
	coldkeySs58, _ := opts.String("<coldkey_ss58>")
	coldkeySs58 = strings.TrimSpace(coldkeySs58)
	coldkey, err := ss58.DecodeWithPrefix(coldkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("invalid ss58 coldkey %q: %w", coldkeySs58, err)
	}
	return coldkeySs58, coldkey, nil
}

func hotkeyWalletCommandHotkey(opts docopt.Opts) (*crv4.Keypair, string, error) {
	seedPath, err := opts.String("--hotkey_seed_file")
	if err != nil || seedPath == "" {
		return nil, "", errors.New("--hotkey_seed_file=<path> is required")
	}
	hotkey, err := loadOperatorHotkey(seedPath)
	return hotkey, seedPath, err
}

// "provider wallet hotkey challenge <coldkey_ss58>".
func hotkeyWalletChallenge(ctx context.Context, opts docopt.Opts, out io.Writer) error {
	base, err := providerStateDir()
	if err != nil {
		return err
	}
	hotkey, seedPath, err := hotkeyWalletCommandHotkey(opts)
	if err != nil {
		return err
	}
	coldkeySs58, coldkey, err := hotkeyWalletColdkey(opts)
	if err != nil {
		return err
	}
	epochs, err := hotkeyWalletEpochsFromOpts(opts)
	if err != nil {
		return err
	}
	target, err := hotkeyWalletTargetFromOpts(opts)
	if err != nil {
		return err
	}
	targets := []hotkeyWalletTarget{target}
	store := hotkeyWalletStore(base)
	statement, err := hotkeyWalletNextStatement(ctx, store, targets, hotkey, coldkey, coldkeySs58, epochs, out)
	if err != nil {
		return err
	}
	if err := store.SetPending(*statement); err != nil {
		return err
	}
	message, err := statement.Message()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Sign this message with the coldkey %s: sr25519, \"substrate\" signing context.\n", coldkeySs58)
	fmt.Fprintf(out, "The bytes to sign are the UTF-8 text between the markers exactly as printed (LF line endings, no trailing newline);\n")
	fmt.Fprintf(out, "a Polkadot extension signRaw of type \"bytes\", which wraps them in <Bytes>...</Bytes>, is accepted too.\n")
	fmt.Fprintf(out, "It is generation %d of the global consent that pays the networks delegated to hotkey %s on every operator of netuid %d to this coldkey, for earning epochs %d through %d.\n", statement.Generation, hotkey.Address(), statement.Subnet.Netuid, statement.FromEpoch, statement.ThroughEpoch)
	fmt.Fprintf(out, "It stays pending in %s until it is set or replaced.\n", filepath.Join(store.Directory, "pending.json"))
	fmt.Fprintf(out, "\n----- message -----\n%s\n----- end -----\n", message)
	fmt.Fprintf(out, "message bytes (hex): 0x%s\n", hex.EncodeToString([]byte(message)))
	fmt.Fprintf(out, "\nThen submit the 64-byte signature as hex:\n")
	fmt.Fprintf(out, "  provider wallet hotkey set %s --hotkey_seed_file=%s --message='%s' --signature=0x<128 hex chars>\n", coldkeySs58, snWalletShellValue(seedPath), snEscapeMessage(message))
	return nil
}

// "provider wallet hotkey set <coldkey_ss58>".
func hotkeyWalletSet(ctx context.Context, opts docopt.Opts, out io.Writer) error {
	base, err := providerStateDir()
	if err != nil {
		return err
	}
	hotkey, _, err := hotkeyWalletCommandHotkey(opts)
	if err != nil {
		return err
	}
	coldkeySs58, coldkey, err := hotkeyWalletColdkey(opts)
	if err != nil {
		return err
	}
	epochs, err := hotkeyWalletEpochsFromOpts(opts)
	if err != nil {
		return err
	}
	seedFile, _ := opts.String("--coldkey_seed_file")
	message, _ := opts.String("--message")
	signatureHex, _ := opts.String("--signature")
	if seedFile == "" && message == "" {
		return errors.New("the coldkey's signature is required: --coldkey_seed_file=<path>, or --message and --signature for the statement provider wallet hotkey challenge printed")
	}
	target, err := hotkeyWalletTargetFromOpts(opts)
	if err != nil {
		return err
	}
	targets := []hotkeyWalletTarget{target}
	store := hotkeyWalletStore(base)
	chain, err := store.Chain()
	if err != nil {
		return err
	}

	var original protocol.HotkeyWalletMappingConsent
	stored := false
	if seedFile != "" {
		coldkeyPair, err := snLoadColdkey(seedFile, coldkeySs58, coldkey)
		if err != nil {
			return err
		}
		// A set whose submission failed has already stored its generation. A
		// retry with the same coldkey and epochs resubmits that head: signing a
		// new generation on every retry would run into the lineage cap.
		reuseHead := false
		if len(chain) > 0 {
			headStatement, _, err := protocol.VerifyHotkeyWalletMappingConsent(ctx, chain[len(chain)-1])
			if err == nil && headStatement.Coldkey == coldkey && headStatement.Hotkey == hotkey.PublicKey() &&
				(epochs == nil || headStatement.FromEpoch == epochs.from && headStatement.ThroughEpoch == epochs.through) {
				original, stored, reuseHead = chain[len(chain)-1], true, true
				fmt.Fprintf(out, "generation %d is already stored for this coldkey and these epochs; resubmitting it\n", headStatement.Generation)
			}
		}
		statement, err := store.Pending()
		if err != nil {
			return err
		}
		if reuseHead {
			// nothing to sign
		} else if statement == nil || statement.Generation != uint64(len(chain))+1 || statement.Hotkey != hotkey.PublicKey() || statement.Coldkey != coldkey || epochs != nil && (statement.FromEpoch != epochs.from || statement.ThroughEpoch != epochs.through) {
			if statement, err = hotkeyWalletNextStatement(ctx, store, targets, hotkey, coldkey, coldkeySs58, epochs, out); err != nil {
				return err
			}
			if err := store.SetPending(*statement); err != nil {
				return err
			}
		}
		if !reuseHead {
			if original.Message, err = statement.Message(); err != nil {
				return err
			}
			// show what the coldkey is about to sign: the epochs of a fresh
			// statement come from the operator
			fmt.Fprintf(out, "signing with the coldkey: generation %d, coldkey %s, epochs %d through %d\n", statement.Generation, coldkeySs58, statement.FromEpoch, statement.ThroughEpoch)
			if original.ColdkeySignature, err = hotkeywallet.Sign(coldkeyPair, original.Message); err != nil {
				return err
			}
		}
	} else {
		original.Message = snUnescapeMessage(message)
		normalized, err := snNormalizeSignatureHex(signatureHex)
		if err != nil {
			return err
		}
		signature, _ := hex.DecodeString(strings.TrimPrefix(normalized, "0x"))
		original.ColdkeySignature = [64]byte(signature)
		if len(chain) > 0 && chain[len(chain)-1].Message == original.Message {
			original.HotkeySignature = chain[len(chain)-1].HotkeySignature
			statement, _, err := protocol.VerifyHotkeyWalletMappingConsent(ctx, original)
			if err != nil {
				return fmt.Errorf("the signature does not verify over the stored head: %w", err)
			}
			if statement.Coldkey != coldkey {
				return fmt.Errorf("the stored head is not a statement for coldkey %s", coldkeySs58)
			}
			original, stored = chain[len(chain)-1], true
		} else {
			pending, err := store.Pending()
			if err != nil {
				return err
			}
			if pending == nil {
				return errors.New("no hotkey wallet statement is pending; run provider wallet hotkey challenge first")
			}
			pendingMessage, err := pending.Message()
			if err != nil {
				return err
			}
			switch {
			case pendingMessage != original.Message:
				return fmt.Errorf("--message is not the pending statement in %s; sign the message that provider wallet hotkey challenge printed last", filepath.Join(store.Directory, "pending.json"))
			case pending.Coldkey != coldkey:
				pendingColdkey, _ := ss58.Encode(pending.Coldkey, ss58.BittensorPrefix)
				return fmt.Errorf("the pending statement names coldkey %s, not %s", pendingColdkey, coldkeySs58)
			case pending.Hotkey != hotkey.PublicKey():
				return errors.New("the pending statement names another hotkey than --hotkey_seed_file")
			case epochs != nil && (pending.FromEpoch != epochs.from || pending.ThroughEpoch != epochs.through):
				return fmt.Errorf("the pending statement earns epochs %d through %d, not %d through %d", pending.FromEpoch, pending.ThroughEpoch, epochs.from, epochs.through)
			}
		}
	}
	if !stored {
		if original.HotkeySignature, err = hotkeywallet.Sign(hotkey, original.Message); err != nil {
			return err
		}
		if _, _, err := protocol.VerifyHotkeyWalletMappingConsent(ctx, original); err != nil {
			return fmt.Errorf("the coldkey's signature does not verify for %s: %w", coldkeySs58, err)
		}
		if err := store.Append(original); err != nil {
			return err
		}
	}
	if chain, err = store.Chain(); err != nil {
		return err
	}
	head, headHash, err := protocol.VerifyHotkeyWalletMappingLineage(ctx, chain)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "hotkey wallet generation %d: coldkey %s, epochs %d through %d, head 0x%x\n", head.Generation, coldkeySs58, head.FromEpoch, head.ThroughEpoch, headHash)
	replaceOther, _ := opts.Bool("--replace-other-hotkey")
	if failed := hotkeyWalletSubmit(ctx, targets, nil, hotkey, chain, replaceOther, out); failed > 0 {
		return fmt.Errorf("%d of %d authenticated operators do not adopt the head yet; set it again with the same --message and --signature", failed, len(targets))
	}
	return nil
}

// "provider wallet hotkey status".
func hotkeyWalletStatus(ctx context.Context, opts docopt.Opts, out io.Writer) error {
	base, err := providerStateDir()
	if err != nil {
		return err
	}
	store := hotkeyWalletStore(base)
	chain, err := store.Chain()
	if err != nil {
		return err
	}
	var hotkey [32]byte
	if seedPath, err := opts.String("--hotkey_seed_file"); err == nil && seedPath != "" {
		keypair, err := loadOperatorHotkey(seedPath)
		if err != nil {
			return err
		}
		hotkey = keypair.PublicKey()
	}
	var head *protocol.HotkeyWalletMappingStatement
	var headHash [32]byte
	if len(chain) == 0 {
		fmt.Fprintf(out, "hotkey wallet chain: none in %s\n", store.Directory)
	} else {
		fmt.Fprintf(out, "hotkey wallet chain in %s:\n", store.Directory)
		for _, original := range chain {
			statement, hash, err := protocol.VerifyHotkeyWalletMappingConsent(ctx, original)
			if err != nil {
				return err
			}
			coldkeySs58, _ := ss58.Encode(statement.Coldkey, ss58.BittensorPrefix)
			fmt.Fprintf(out, "  generation %d: coldkey %s, epochs %d through %d, original 0x%x\n", statement.Generation, coldkeySs58, statement.FromEpoch, statement.ThroughEpoch, hash)
			head, headHash = statement, hash
		}
		hotkeySs58, _ := ss58.Encode(head.Hotkey, ss58.BittensorPrefix)
		fmt.Fprintf(out, "head: generation %d, 0x%x, hotkey %s, chain %d netuid %d\n", head.Generation, headHash, hotkeySs58, head.Subnet.ChainID, head.Subnet.Netuid)
		if hotkey != ([32]byte{}) && hotkey != head.Hotkey {
			fmt.Fprintln(out, "warning: --hotkey_seed_file is not the chain's hotkey")
		}
		hotkey = head.Hotkey
	}
	if pending, err := store.Pending(); err != nil {
		return err
	} else if pending != nil {
		coldkeySs58, _ := ss58.Encode(pending.Coldkey, ss58.BittensorPrefix)
		fmt.Fprintf(out, "pending: generation %d for coldkey %s, epochs %d through %d, awaiting the coldkey's signature\n", pending.Generation, coldkeySs58, pending.FromEpoch, pending.ThroughEpoch)
	}
	target, err := hotkeyWalletTargetFromOpts(opts)
	if err != nil {
		return err
	}
	targets := []hotkeyWalletTarget{target}
	for _, t := range targets {
		network, entry, err := hotkeyWalletEntries(ctx, t)
		switch {
		case err != nil:
			fmt.Fprintf(out, "operator %s: unavailable: %v\n", t.domain, err)
		case entry == nil:
			fmt.Fprintf(out, "operator %s: no hotkey delegation\n", t.domain)
		case head != nil && entry.adopts(hotkey, headHash, head.Generation):
			fmt.Fprintf(out, "operator %s: adopts the head, generation %d, from epoch %d through %d\n", t.domain, head.Generation, entry.FromEpoch, entry.ThroughEpoch)
			epochResult, epochErr := hotkeyWalletEpoch(ctx, t)
			var epoch uint64
			if epochErr == nil {
				epoch = epochResult.Epoch
			}
			for _, note := range hotkeyWalletDelegationNotes(entry, network, epoch, epochErr == nil) {
				fmt.Fprintln(out, note)
			}
		default:
			fmt.Fprintf(out, "operator %s: delegates to hotkey %s at generation %d, not the head\n", t.domain, entry.HotkeySs58, entry.ConsentGeneration)
		}
	}
	return nil
}

// hotkeyWalletDelegationNotes are the caveats `hotkey status` adds to a
// delegation that adopts the head: the server pays the provider's own signed
// consent first, then a signed network consent, and the hotkey's coldkey only
// after both; and a delegation outside its window earns nothing.
func hotkeyWalletDelegationNotes(delegation *hotkeyWalletEntry, network *hotkeyWalletEntry, epoch uint64, epochOK bool) []string {
	var notes []string
	if epochOK && delegation.ThroughEpoch < epoch+2 {
		notes = append(notes, fmt.Sprintf("  not effective: the window ends at epoch %d and the operator is at epoch %d; the next provide upkeep renews it", delegation.ThroughEpoch, epoch))
	}
	if network != nil {
		notes = append(notes, fmt.Sprintf("  a signed network wallet consent (coldkey %s) outranks this delegation, so that coldkey is paid, not the hotkey's", network.ColdkeySs58))
	}
	notes = append(notes, "  a per-provider signed consent outranks both for that provider (not listed here)")
	return notes
}

// "provider wallet hotkey challenge|set|status".
func hotkeyWalletCmd(opts docopt.Opts) {
	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(event.Ctx())
	var err error
	if challenge, _ := opts.Bool("challenge"); challenge {
		err = hotkeyWalletChallenge(ctx, opts, os.Stdout)
	} else if set, _ := opts.Bool("set"); set {
		err = hotkeyWalletSet(ctx, opts, os.Stdout)
	} else {
		err = hotkeyWalletStatus(ctx, opts, os.Stdout)
	}
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wallet hotkey: %v\n", err)
		os.Exit(1)
	}
}

// hotkeyWalletUpkeepStepTimeout bounds one whole upkeep step. A step makes
// several requests (list, consent, delegation, accept), each already capped by
// the 30s per-request client timeout, so the step budget must exceed that or a
// slow first request starves the rest.
const hotkeyWalletUpkeepStepTimeout = 5 * time.Minute

// Single step of hourly upkeep.
func runHotkeyWalletUpkeepStep(ctx context.Context, apiUrl string, hotkey *crv4.Keypair, lastGen *uint64) (hotkeyWalletOutcome, error) {
	base, err := providerStateDir()
	if err != nil {
		return hotkeyWalletOutcome{}, err
	}
	store := hotkeyWalletStore(base)
	chain, err := store.Chain()
	if err != nil {
		return hotkeyWalletOutcome{}, err
	}
	if len(chain) == 0 {
		return hotkeyWalletOutcome{}, nil
	}

	byJwt, err := readNetworkJwt()
	if err != nil {
		return hotkeyWalletOutcome{}, err
	}
	target := hotkeyWalletSingleTarget(apiUrl, byJwt)

	callCtx, cancel := context.WithTimeout(ctx, hotkeyWalletUpkeepStepTimeout)
	defer cancel()

	// the hourly upkeep never replaces another hotkey's delegation
	return ensureOperatorHotkeyWallet(callCtx, target, hotkey, chain, false)
}

// Hourly delegation refresh loop started from provide.
func runHotkeyWalletUpkeep(ctx context.Context, apiUrl string, hotkey *crv4.Keypair) {
	var lastGen uint64
	emptyChainLogged := false

	// First run 60s after startup
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		outcome, err := runHotkeyWalletUpkeepStep(ctx, apiUrl, hotkey, &lastGen)
		if err != nil {
			errStr := err.Error()
			if len(errStr) > 200 {
				errStr = errStr[:200]
			}
			tlog("[wallet] hotkey delegation not refreshed: %s; retry in 1h\n", errStr)
		} else {
			if outcome.generation == 0 {
				if !emptyChainLogged {
					tlog("[wallet] hotkey wallet chain empty; skipping upkeep\n")
					emptyChainLogged = true
				}
			} else {
				emptyChainLogged = false
				if outcome.delegated || outcome.generation != lastGen {
					tlog("[wallet] hotkey delegation refreshed: generation %d from epoch %d through %d\n", outcome.generation, outcome.fromEpoch, outcome.throughEpoch)
					lastGen = outcome.generation
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startHotkeyWalletUpkeep starts the hourly delegation refresh under the loop
// supervisor when provide was given a hotkey seed file. It never blocks or
// fails provide: a missing or unsafe seed file is reported to the caller to log
// (the error never carries the seed) and the loop is simply not started. run is
// the loop body, injectable for tests.
func startHotkeyWalletUpkeep(ctx context.Context, seedFile string, apiUrl string, run func(context.Context, string, *crv4.Keypair)) (bool, error) {
	if seedFile == "" {
		return false, nil
	}
	hotkey, err := loadOperatorHotkey(seedFile)
	if err != nil {
		return false, fmt.Errorf("hotkey seed file %s: %w", seedFile, err)
	}
	go superviseLoop(ctx, "hotkey_wallet_upkeep", func() { run(ctx, apiUrl, hotkey) }, nil)
	return true, nil
}

// snLoadColdkey loads the coldkey seed (never creating the file) and refuses
// one that does not derive the given address.
func snLoadColdkey(seedFile string, coldkeySs58 string, pubkey [32]byte) (*crv4.Keypair, error) {
	seed, err := crv4.LoadSeedFile(seedFile)
	if err != nil {
		return nil, fmt.Errorf("coldkey seed file %s: %w", seedFile, err)
	}
	keypair, err := crv4.KeypairFromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("coldkey seed file %s: %w", seedFile, err)
	}
	if keypair.PublicKey() != pubkey {
		return nil, fmt.Errorf("coldkey seed file %s derives %s, not %s", seedFile, keypair.Address(), coldkeySs58)
	}
	return keypair, nil
}

// snNormalizeSignatureHex checks a signature given on the command line (64
// bytes of hex, 0x optional) and returns it 0x-prefixed.
func snNormalizeSignatureHex(signature string) (string, error) {
	clean := strings.TrimSpace(signature)
	clean = strings.TrimPrefix(strings.TrimPrefix(clean, "0x"), "0X")
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("--signature must be hex: %w", err)
	}
	if len(raw) != 64 {
		return "", fmt.Errorf("--signature must be a 64-byte sr25519 signature, got %d bytes", len(raw))
	}
	return "0x" + hex.EncodeToString(raw), nil
}

// snEscapeMessage is the one-line form of a challenge: a literal \n per line.
func snEscapeMessage(message string) string {
	return strings.ReplaceAll(message, "\n", `\n`)
}

// snUnescapeMessage inverts snEscapeMessage. A message passed with real
// newlines is left as it is.
func snUnescapeMessage(message string) string {
	if strings.Contains(message, "\n") {
		return message
	}
	return strings.ReplaceAll(message, `\n`, "\n")
}

// Preserve the selected endpoint and private file even when a shell path
// contains spaces or quotes. This is display text, never executed locally.
func snWalletShellValue(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// Never creates a hotkey: a missing or unsafe seed file is an error naming
// its path.
func loadOperatorHotkey(path string) (*crv4.Keypair, error) {
	seed, err := crv4.LoadSeedFile(path)
	if err != nil {
		return nil, fmt.Errorf("hotkey seed file %s: %w", path, err)
	}
	return crv4.KeypairFromSeed(seed)
}
