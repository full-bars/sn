package clientauth

// URNETWORK_HOT_RESTART=0 makes every provider start a fresh identity: the
// operator's contract is a new client_id per start, while hot restart on
// keeps the retained identity. LoadOrRegisterClientJwt cannot express the off
// case: it owns retained-custody recovery, and therefore reads and refreshes
// the per-slot credential. This file is the explicit alternative the
// provider's hot-restart-off branch uses, and it never reads that credential.
// A completed fresh registration leaves the slot's custody consistent for the
// identity it registered, so a later retained-custody start adopts it without
// any refusal.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Fresh provider creation is intentionally separate from the versioned
// registration operation. The versioned request binds exactly one client per
// key/slot scope on the server, so it can only replay or refuse, never
// allocate the new identity hot restart off promises. The legacy no-client
// creation call is the v33 fresh mint, and the new client becomes the slot's
// adopted custody (an existing-client marker, never a versioned record).
func registerFreshClientJwtWithCustody(ctx context.Context, api *sdk.Api, networkPath, clientPath, description string, scope RegistrationScope, allowCreate bool, directory *registrationStore) (_ string, _ connect.Id, returnErr error) {
	if ctx == nil || api == nil || directory == nil {
		return "", connect.Id{}, errors.New("fresh registration has no operation owner")
	}
	if err := ctx.Err(); err != nil {
		return "", connect.Id{}, err
	}
	endpoint, err := api.NetworkClientRegistrationEndpoint()
	if err != nil {
		return "", connect.Id{}, err
	}
	if err := validateFreshProviderScope(endpoint, scope); err != nil {
		return "", connect.Id{}, err
	}
	// A fresh identity is new client creation: only a caller with explicit
	// creation authority may allocate. Without it the retained identity stays
	// untouched and the operator sees why, instead of a silent allocation or
	// a silent refresh of the identity the switch asked to replace.
	if !allowCreate {
		return "", connect.Id{}, &RegistrationRefusedError{Code: "fresh_identity_requires_explicit_creation"}
	}
	if filepath.Dir(networkPath) != filepath.Dir(clientPath) || filepath.Clean(networkPath) != networkPath || !filepath.IsAbs(networkPath) {
		return "", connect.Id{}, errors.New("fresh registration bootstrap differs from its owned custody directory")
	}
	owner, err := openRegistrationStoreForOwner(clientPath, directory)
	if err != nil {
		return "", connect.Id{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, owner.close()) }()

	clientName := filepath.Base(clientPath)
	recordName := clientName + ".registration"
	anchorName := recordName + ".started"
	legacyName := recordName + ".existing"

	// The retained credential is deliberately not read: refreshing it is the
	// divergence this path exists to remove. The rejection tombstone still is
	// read: a confirmed rejection is sticky in this original custody, and no
	// automatic restart may clear it by allocating a replacement.
	if _, err := owner.read(filepath.Base(rejectionPath(clientPath))); err == nil {
		return "", connect.Id{}, &RegistrationRefusedError{Code: "client_revoked"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", connect.Id{}, err
	}
	raw, err := owner.read(filepath.Base(networkPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", connect.Id{}, &RegistrationRefusedError{Code: "network_authentication_missing"}
		}
		return "", connect.Id{}, err
	}
	bootstrap := strings.TrimSpace(string(raw))
	if bootstrap == "" {
		return "", connect.Id{}, errors.New("network sign-in credential is empty")
	}
	bootstrapIdentity, err := registrationIdentity(bootstrap)
	if err != nil || !validRegistrationIdentityId(bootstrapIdentity.NetworkId) || !validRegistrationIdentityId(bootstrapIdentity.UserId) || bootstrapIdentity.clientId != "" || bootstrapIdentity.deviceId != "" {
		return "", connect.Id{}, errors.New("registration bootstrap does not name a network principal")
	}
	api.SetByJwt(bootstrap)
	if err := owner.check(); err != nil {
		return "", connect.Id{}, err
	}
	// The legacy route allocates a new client on every call, which is the only
	// route that can: the versioned scope already owns one client. Provide
	// intent matches the v33 fresh mint and keeps a provider install exempt
	// from the concurrent top-level client limit, which per-restart fresh
	// identities would otherwise spend.
	result, err := api.AuthNetworkClientSyncWithContext(ctx, &sdk.AuthNetworkClientArgs{
		DeviceDescription: description,
		DeviceSpec:        "",
		ProvideIntent:     true,
	})
	if err != nil {
		// the request carried the bootstrap: a 401 rejects the network sign-in
		return "", connect.Id{}, networkCredentialRejection(err)
	}
	if result == nil {
		return "", connect.Id{}, errors.New("fresh registration returned a null client result")
	}
	if result.Error != nil {
		code := "authentication_refused"
		if result.Error.ClientLimitExceeded {
			code = "client_limit"
		}
		return "", connect.Id{}, &RegistrationRefusedError{Code: code}
	}
	if result.ByClientJwt == "" {
		return "", connect.Id{}, errors.New("fresh registration returned an empty client JWT")
	}
	fresh, err := registrationIdentity(result.ByClientJwt)
	if err != nil || !validRegistrationIdentityId(fresh.clientId) || !validRegistrationIdentityId(fresh.deviceId) {
		return "", connect.Id{}, &RegistrationResponseIdentityError{}
	}
	// The reply must not switch the session to another client or principal
	// than the one the bootstrap selected, and its own client id (when sent)
	// must agree with the credential it issued.
	if result.ClientId != nil && result.ClientId.String() != fresh.clientId || !fresh.registrationPrincipal.equal(bootstrapIdentity.registrationPrincipal) {
		return "", connect.Id{}, &RegistrationResponseIdentityError{}
	}

	// The completed allocation precedes custody reconciliation: a crash
	// before the first removal leaves the original record and credential
	// exactly as a retained-custody start expects.
	//
	// The abandoned versioned registration and its anchor are removed first:
	// the new identity is adopted custody (the server has no versioned
	// operation for it), and a record can never coexist with the
	// existing-client custody published below ("registration has both
	// original creation and existing-client custody"), neither now nor after
	// the next write is interrupted.
	for _, name := range []string{recordName, anchorName} {
		if err := owner.remove(name); err != nil {
			return "", connect.Id{}, err
		}
	}
	// The credential is installed before its existing-client marker, so an
	// interrupted handoff leaves either the old custody intact or a complete
	// credential the retained-custody path adopts from the token itself. The
	// marker may never name an identity the credential file does not have.
	if err := owner.write(clientName, []byte(result.ByClientJwt)); err != nil {
		return "", connect.Id{}, err
	}
	marker, err := existingClientCustodyMarker(scope, fresh)
	if err != nil {
		return "", connect.Id{}, err
	}
	if err := owner.write(legacyName, marker); err != nil {
		return "", connect.Id{}, err
	}
	api.SetByJwt(result.ByClientJwt)
	id, err := connect.ParseId(fresh.clientId)
	if err != nil {
		return "", connect.Id{}, &RegistrationResponseIdentityError{}
	}
	return result.ByClientJwt, id, nil
}

// The fresh path serves exactly the provider operation; chain, validator and
// deployment authority mixed into its scope is refused before any read or
// request, and the slot naming matches the versioned provider wrapper.
func validateFreshProviderScope(endpoint string, scope RegistrationScope) error {
	if scope.Endpoint != endpoint || scope.ClientRole != "provider-v1" ||
		scope.DeploymentId != "" || scope.ChainId != 0 || scope.GenesisHash != "" || scope.Netuid != 0 || scope.ValidatorId != 0 || scope.OperatorNoId != 0 {
		return errors.New("fresh registration scope differs from its configured API or provider role")
	}
	if scope.ClientSlot != "direct" {
		slot, err := hex.DecodeString(strings.TrimPrefix(scope.ClientSlot, "proxy-sha256:"))
		if err != nil || len(slot) != 32 || scope.ClientSlot != "proxy-sha256:"+hex.EncodeToString(slot) {
			return errors.New("fresh registration service slot is invalid")
		}
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(scope.ClientKey, "0x"))
	if err != nil || len(raw) != 32 || bytes.Equal(raw, make([]byte, 32)) || scope.ClientKey != "0x"+hex.EncodeToString(raw) {
		return errors.New("fresh registration scope has an invalid client key")
	}
	return nil
}
