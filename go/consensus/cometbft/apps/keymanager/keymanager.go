package keymanager

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/cometbft/cometbft/abci/types"
	"golang.org/x/crypto/sha3"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/consensus/api/transaction"
	tmapi "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/api"
	keymanagerState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/keymanager/state"
	registryapp "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry"
	registryState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/registry/state"
	stakingState "github.com/oasisprotocol/oasis-core/go/consensus/cometbft/apps/staking/state"
	"github.com/oasisprotocol/oasis-core/go/keymanager/api"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
)

// minProposalReplicationPercent is the minimum percentage of enclaves in the key manager committee
// that must replicate the proposal for the next master secret before it is accepted.
const minProposalReplicationPercent = 66

var emptyHashSha3 = sha3.Sum256(nil)

type keymanagerApplication struct {
	state tmapi.ApplicationState
}

func (app *keymanagerApplication) Name() string {
	return AppName
}

func (app *keymanagerApplication) ID() uint8 {
	return AppID
}

func (app *keymanagerApplication) Methods() []transaction.MethodName {
	return api.Methods
}

func (app *keymanagerApplication) Blessed() bool {
	return false
}

func (app *keymanagerApplication) Dependencies() []string {
	return []string{registryapp.AppName}
}

func (app *keymanagerApplication) OnRegister(state tmapi.ApplicationState, md tmapi.MessageDispatcher) {
	app.state = state
}

func (app *keymanagerApplication) OnCleanup() {}

func (app *keymanagerApplication) BeginBlock(ctx *tmapi.Context) error {
	if changed, epoch := app.state.EpochChanged(ctx); changed {
		return app.onEpochChange(ctx, epoch)
	}
	return nil
}

func (app *keymanagerApplication) ExecuteMessage(ctx *tmapi.Context, kind, msg interface{}) (interface{}, error) {
	return nil, fmt.Errorf("keymanager: unexpected message")
}

func (app *keymanagerApplication) ExecuteTx(ctx *tmapi.Context, tx *transaction.Transaction) error {
	state := keymanagerState.NewMutableState(ctx.State())

	ctx.SetPriority(AppPriority)

	switch tx.Method {
	case api.MethodUpdatePolicy:
		var sigPol api.SignedPolicySGX
		if err := cbor.Unmarshal(tx.Body, &sigPol); err != nil {
			return api.ErrInvalidArgument
		}
		return app.updatePolicy(ctx, state, &sigPol)
	case api.MethodPublishMasterSecret:
		var sigSec api.SignedEncryptedMasterSecret
		if err := cbor.Unmarshal(tx.Body, &sigSec); err != nil {
			return api.ErrInvalidArgument
		}
		return app.publishMasterSecret(ctx, state, &sigSec)
	case api.MethodPublishEphemeralSecret:
		var sigSec api.SignedEncryptedEphemeralSecret
		if err := cbor.Unmarshal(tx.Body, &sigSec); err != nil {
			return api.ErrInvalidArgument
		}
		return app.publishEphemeralSecret(ctx, state, &sigSec)
	default:
		return fmt.Errorf("keymanager: invalid method: %s", tx.Method)
	}
}

func (app *keymanagerApplication) EndBlock(ctx *tmapi.Context) (types.ResponseEndBlock, error) {
	return types.ResponseEndBlock{}, nil
}

func (app *keymanagerApplication) onEpochChange(ctx *tmapi.Context, epoch beacon.EpochTime) error {
	// Query the runtime and node lists.
	regState := registryState.NewMutableState(ctx.State())
	runtimes, _ := regState.Runtimes(ctx)
	nodes, _ := regState.Nodes(ctx)
	registry.SortNodeList(nodes)

	params, err := regState.ConsensusParameters(ctx)
	if err != nil {
		return fmt.Errorf("failed to get consensus parameters: %w", err)
	}

	var stakeAcc *stakingState.StakeAccumulatorCache
	if !params.DebugBypassStake {
		stakeAcc, err = stakingState.NewStakeAccumulatorCache(ctx)
		if err != nil {
			return fmt.Errorf("failed to create stake accumulator cache: %w", err)
		}
		defer stakeAcc.Discard()
	}

	// Recalculate all the key manager statuses.
	//
	// Note: This assumes that once a runtime is registered, it never expires.
	var toEmit []*api.Status
	state := keymanagerState.NewMutableState(ctx.State())
	for _, rt := range runtimes {
		if rt.Kind != registry.KindKeyManager {
			continue
		}

		// Suspend the runtime in case the registering entity no longer has enough stake to cover
		// the entity and runtime deposits.
		if !params.DebugBypassStake && rt.GovernanceModel != registry.GovernanceConsensus {
			acctAddr := rt.StakingAddress()
			if acctAddr == nil {
				// This should never happen.
				ctx.Logger().Error("unknown runtime governance model",
					"rt_id", rt.ID,
					"gov_model", rt.GovernanceModel,
				)
				return fmt.Errorf("unknown runtime governance model on runtime %s: %s", rt.ID, rt.GovernanceModel)
			}

			if err = stakeAcc.CheckStakeClaims(*acctAddr); err != nil {
				ctx.Logger().Debug("insufficient stake for key manager runtime operation",
					"err", err,
					"entity", rt.EntityID,
					"account", *acctAddr,
				)

				// Suspend runtime.
				if err := regState.SuspendRuntime(ctx, rt.ID); err != nil {
					return err
				}

				continue
			}
		}

		var forceEmit bool
		oldStatus, err := state.Status(ctx, rt.ID)
		switch err {
		case nil:
		case api.ErrNoSuchStatus:
			// This must be a new key manager runtime.
			forceEmit = true
			oldStatus = &api.Status{
				ID: rt.ID,
			}
		default:
			// This is fatal, as it suggests state corruption.
			ctx.Logger().Error("failed to query key manager status",
				"id", rt.ID,
				"err", err,
			)
			return fmt.Errorf("failed to query key manager status: %w", err)
		}

		secret, err := state.MasterSecret(ctx, rt.ID)
		if err != nil && err != api.ErrNoSuchMasterSecret {
			ctx.Logger().Error("failed to query key manager master secret",
				"id", rt.ID,
				"err", err,
			)
			return fmt.Errorf("failed to query key manager master secret: %w", err)
		}

		newStatus := app.generateStatus(ctx, rt, oldStatus, secret, nodes, params, epoch)
		if forceEmit || !bytes.Equal(cbor.Marshal(oldStatus), cbor.Marshal(newStatus)) {
			ctx.Logger().Debug("status updated",
				"id", newStatus.ID,
				"is_initialized", newStatus.IsInitialized,
				"is_secure", newStatus.IsSecure,
				"generation", newStatus.Generation,
				"rotation_epoch", newStatus.RotationEpoch,
				"checksum", hex.EncodeToString(newStatus.Checksum),
				"rsk", newStatus.RSK,
				"nodes", newStatus.Nodes,
			)

			// Set, enqueue for emit.
			if err = state.SetStatus(ctx, newStatus); err != nil {
				return fmt.Errorf("failed to set key manager status: %w", err)
			}
			toEmit = append(toEmit, newStatus)
		}
	}

	// Note: It may be a good idea to sweep statuses that don't have runtimes,
	// but as runtime registrations last forever, so this shouldn't be possible.

	// Emit the update event if required.
	if len(toEmit) > 0 {
		ctx.EmitEvent(tmapi.NewEventBuilder(app.Name()).TypedAttribute(&api.StatusUpdateEvent{
			Statuses: toEmit,
		}))
	}

	return nil
}

func (app *keymanagerApplication) generateStatus( // nolint: gocyclo
	ctx *tmapi.Context,
	kmrt *registry.Runtime,
	oldStatus *api.Status,
	secret *api.SignedEncryptedMasterSecret,
	nodes []*node.Node,
	params *registry.ConsensusParameters,
	epoch beacon.EpochTime,
) *api.Status {
	status := &api.Status{
		ID:            kmrt.ID,
		IsInitialized: oldStatus.IsInitialized,
		IsSecure:      oldStatus.IsSecure,
		Generation:    oldStatus.Generation,
		RotationEpoch: oldStatus.RotationEpoch,
		Checksum:      oldStatus.Checksum,
		Policy:        oldStatus.Policy,
	}

	// Data needed to count the nodes that have replicated the proposal for the next master secret.
	var (
		nextGeneration uint64
		nextChecksum   []byte
		nextRSK        *signature.PublicKey
		updatedNodes   []signature.PublicKey
	)
	nextGeneration = status.NextGeneration()
	if secret != nil && secret.Secret.Generation == nextGeneration && secret.Secret.Epoch == epoch {
		nextChecksum = secret.Secret.Secret.Checksum
	}

	// Compute the policy hash to reject nodes that are not up-to-date.
	var rawPolicy []byte
	if status.Policy != nil {
		rawPolicy = cbor.Marshal(status.Policy)
	}
	policyHash := sha3.Sum256(rawPolicy)

	ts := ctx.Now()
	height := uint64(ctx.BlockHeight())

	// Construct a key manager committee. A node is added to the committee if it supports
	// at least one version of the key manager runtime and if all supported versions conform
	// to the key manager status fields.
nextNode:
	for _, n := range nodes {
		if n.IsExpired(uint64(epoch)) {
			continue
		}
		if !n.HasRoles(node.RoleKeyManager) {
			continue
		}

		secretReplicated := true
		isInitialized := status.IsInitialized
		isSecure := status.IsSecure
		RSK := status.RSK
		nRSK := nextRSK

		var numVersions int
		for _, nodeRt := range n.Runtimes {
			if !nodeRt.ID.Equal(&kmrt.ID) {
				continue
			}

			vars := []interface{}{
				"id", kmrt.ID,
				"node_id", n.ID,
				"version", nodeRt.Version,
			}

			var teeOk bool
			if nodeRt.Capabilities.TEE == nil {
				teeOk = kmrt.TEEHardware == node.TEEHardwareInvalid
			} else {
				teeOk = kmrt.TEEHardware == nodeRt.Capabilities.TEE.Hardware
			}
			if !teeOk {
				ctx.Logger().Error("TEE hardware mismatch", vars...)
				continue nextNode
			}

			initResponse, err := api.VerifyExtraInfo(ctx.Logger(), n.ID, kmrt, nodeRt, ts, height, params)
			if err != nil {
				ctx.Logger().Error("failed to validate ExtraInfo", append(vars, "err", err)...)
				continue nextNode
			}

			// Skip nodes with mismatched policy.
			var nodePolicyHash [api.ChecksumSize]byte
			switch len(initResponse.PolicyChecksum) {
			case 0:
				nodePolicyHash = emptyHashSha3
			case api.ChecksumSize:
				copy(nodePolicyHash[:], initResponse.PolicyChecksum)
			default:
				ctx.Logger().Error("failed to parse policy checksum", append(vars, "err", err)...)
				continue nextNode
			}
			if policyHash != nodePolicyHash {
				ctx.Logger().Error("Policy checksum mismatch for runtime", vars...)
				continue nextNode
			}

			// Set immutable status fields that cannot change after initialization.
			if !isInitialized {
				// The first version gets to be the source of truth.
				isInitialized = true
				isSecure = initResponse.IsSecure
			}

			// Skip nodes with mismatched status fields.
			if initResponse.IsSecure != isSecure {
				ctx.Logger().Error("Security status mismatch for runtime", vars...)
				continue nextNode
			}

			// Skip nodes with mismatched checksum.
			// Note that a node needs to register with an empty checksum if no master secrets
			// have been generated so far. Otherwise, if secrets have been generated, the node
			// needs to register with a checksum computed over all the secrets generated so far
			// since the key manager's checksum is updated after every master secret rotation.
			if !bytes.Equal(initResponse.Checksum, status.Checksum) {
				ctx.Logger().Error("Checksum mismatch for runtime", vars...)
				continue nextNode
			}

			// Update mutable status fields that can change on epoch transitions.
			if RSK == nil {
				// The first version with non-nil runtime signing key gets to be the source of truth.
				RSK = initResponse.RSK
			}

			// Skip nodes with mismatched runtime signing key.
			// For backward compatibility we always allow nodes without runtime signing key.
			if initResponse.RSK != nil && !initResponse.RSK.Equal(*RSK) {
				ctx.Logger().Error("Runtime signing key mismatch for runtime", vars)
				continue nextNode
			}

			// Check if all versions have replicated the last master secret,
			// derived the same RSK and are ready to move to the next generation.
			if !bytes.Equal(initResponse.NextChecksum, nextChecksum) {
				secretReplicated = false
			}
			if nRSK == nil {
				nRSK = initResponse.NextRSK
			}
			if initResponse.NextRSK != nil && !initResponse.NextRSK.Equal(*nRSK) {
				secretReplicated = false
			}

			numVersions++
		}

		if numVersions == 0 {
			continue
		}
		if !isInitialized {
			panic("the key manager must be initialized")
		}
		if secretReplicated {
			nextRSK = nRSK
			updatedNodes = append(updatedNodes, n.ID)
		}

		// If the key manager is not initialized, the first verified node gets to be the source
		// of truth, every other node will sync off it.
		if !status.IsInitialized {
			status.IsInitialized = true
			status.IsSecure = isSecure
		}
		status.RSK = RSK
		status.Nodes = append(status.Nodes, n.ID)
	}

	// Accept the proposal if the majority of the nodes have replicated
	// the proposal for the next master secret.
	if numNodes := len(status.Nodes); numNodes > 0 && nextChecksum != nil {
		percent := len(updatedNodes) * 100 / numNodes
		if percent >= minProposalReplicationPercent {
			status.Generation = nextGeneration
			status.RotationEpoch = epoch
			status.Checksum = nextChecksum
			status.RSK = nextRSK
			status.Nodes = updatedNodes
		}
	}

	return status
}

// New constructs a new keymanager application instance.
func New() tmapi.Application {
	return &keymanagerApplication{}
}
