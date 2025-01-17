package runtime

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"github.com/oasisprotocol/curve25519-voi/primitives/x25519"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common"
	"github.com/oasisprotocol/oasis-core/go/common/cbor"
	"github.com/oasisprotocol/oasis-core/go/common/sgx"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	keymanager "github.com/oasisprotocol/oasis-core/go/keymanager/api"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/env"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/oasis"
	"github.com/oasisprotocol/oasis-core/go/oasis-test-runner/oasis/cli"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
)

// KeyManagerStatus returns the latest key manager status.
func (sc *Scenario) KeyManagerStatus(ctx context.Context) (*keymanager.Status, error) {
	return sc.Net.Controller().Keymanager.GetStatus(ctx, &registry.NamespaceQuery{
		Height: consensus.HeightLatest,
		ID:     KeyManagerRuntimeID,
	})
}

// MasterSecret returns the key manager master secret.
func (sc *Scenario) MasterSecret(ctx context.Context) (*keymanager.SignedEncryptedMasterSecret, error) {
	secret, err := sc.Net.Controller().Keymanager.GetMasterSecret(ctx, &registry.NamespaceQuery{
		Height: consensus.HeightLatest,
		ID:     KeyManagerRuntimeID,
	})
	if err == keymanager.ErrNoSuchMasterSecret {
		return nil, nil
	}
	return secret, err
}

// WaitMasterSecret waits until the specified generation of the master secret is generated.
func (sc *Scenario) WaitMasterSecret(ctx context.Context, generation uint64) (*keymanager.Status, error) {
	sc.Logger.Info("waiting for master secret", "generation", generation)

	mstCh, mstSub, err := sc.Net.Controller().Keymanager.WatchMasterSecrets(ctx)
	if err != nil {
		return nil, err
	}
	defer mstSub.Close()

	stCh, stSub, err := sc.Net.Controller().Keymanager.WatchStatuses(ctx)
	if err != nil {
		return nil, err
	}
	defer stSub.Close()

	var last *keymanager.Status
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case secret := <-mstCh:
			if !secret.Secret.ID.Equal(&KeyManagerRuntimeID) {
				continue
			}

			sc.Logger.Info("master secret proposed",
				"generation", secret.Secret.Generation,
				"epoch", secret.Secret.Epoch,
				"num_ciphertexts", len(secret.Secret.Secret.Ciphertexts),
			)
		case status := <-stCh:
			if !status.ID.Equal(&KeyManagerRuntimeID) {
				continue
			}
			if status.NextGeneration() == 0 {
				continue
			}
			if last != nil && status.Generation == last.Generation {
				last = status
				continue
			}

			sc.Logger.Info("master secret rotation",
				"generation", status.Generation,
				"rotation_epoch", status.RotationEpoch,
			)

			if status.Generation >= generation {
				return status, nil
			}
			last = status
		}
	}
}

// WaitEphemeralSecrets waits for the specified number of ephemeral secrets to be generated.
func (sc *Scenario) WaitEphemeralSecrets(ctx context.Context, n int) (*keymanager.SignedEncryptedEphemeralSecret, error) {
	sc.Logger.Info("waiting ephemeral secrets", "n", n)

	ephCh, ephSub, err := sc.Net.Controller().Keymanager.WatchEphemeralSecrets(ctx)
	if err != nil {
		return nil, err
	}
	defer ephSub.Close()

	var secret *keymanager.SignedEncryptedEphemeralSecret
	for i := 0; i < n; i++ {
		select {
		case secret = <-ephCh:
			sc.Logger.Info("ephemeral secret published",
				"epoch", secret.Secret.Epoch,
			)
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for ephemeral secrets")
		}
	}
	return secret, nil
}

// UpdateRotationInterval updates the master secret rotation interval in the key manager policy.
func (sc *Scenario) UpdateRotationInterval(ctx context.Context, childEnv *env.Env, cli *cli.Helpers, rotationInterval beacon.EpochTime, nonce uint64) error {
	sc.Logger.Info("updating master secret rotation interval in the key manager policy",
		"interval", rotationInterval,
	)

	status, err := sc.KeyManagerStatus(ctx)
	if err != nil && err != keymanager.ErrNoSuchStatus {
		return err
	}

	var policies map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX
	if status != nil && status.Policy != nil {
		policies = status.Policy.Policy.Enclaves
	}

	if err := sc.ApplyKeyManagerPolicy(ctx, childEnv, cli, rotationInterval, policies, nonce); err != nil {
		return err
	}

	return nil
}

// CompareLongtermPublicKeys compares long-term public keys generated by the specified
// key manager nodes.
func (sc *Scenario) CompareLongtermPublicKeys(ctx context.Context, idxs []int) error {
	chainContext, err := sc.Net.Controller().Consensus.GetChainContext(ctx)
	if err != nil {
		return err
	}

	status, err := sc.KeyManagerStatus(ctx)
	if err != nil {
		return err
	}

	var generation uint64
	if status.Generation > 0 {
		// Avoid verification problems when the consensus verifier is one block behind.
		generation = status.Generation - 1
	}

	sc.Logger.Info("comparing long-term public keys generated by the key managers",
		"ids", idxs,
		"generation", generation,
	)

	keys := make(map[uint64]*x25519.PublicKey)
	kms := sc.Net.Keymanagers()
	for _, idx := range idxs {
		km := kms[idx]

		// Prepare an RPC client which will be used to query key manager nodes
		// for public ephemeral keys.
		rpcClient, err := newKeyManagerRPCClient(chainContext)
		if err != nil {
			return err
		}
		peerID, err := rpcClient.addKeyManagerAddrToHost(km)
		if err != nil {
			return err
		}

		for gen := uint64(0); gen <= generation; gen++ {
			sc.Logger.Info("fetching public key", "generation", gen, "node", km.Name)

			var key *x25519.PublicKey
			key, err = rpcClient.fetchPublicKey(ctx, gen, peerID)
			switch {
			case err != nil:
				return err
			case key == nil:
				return fmt.Errorf("master secret generation %d not found", gen)
			}

			if expected, ok := keys[gen]; ok && !bytes.Equal(expected[:], key[:]) {
				return fmt.Errorf("derived keys don't match: expected %+X, given %+X", expected, key)
			}
			keys[gen] = key

			sc.Logger.Info("public key fetched", "key", fmt.Sprintf("%+X", key))
		}
		if err != nil {
			return err
		}
	}
	if expected, size := int(generation)+1, len(keys); expected != size {
		return fmt.Errorf("the number of derived keys doesn't match: expected %d, found %d", expected, size)
	}

	return nil
}

// KeymanagerInitResponse returns InitResponse of the specified key manager node.
func (sc *Scenario) KeymanagerInitResponse(ctx context.Context, idx int) (*keymanager.InitResponse, error) {
	kms := sc.Net.Keymanagers()
	if kmLen := len(kms); kmLen <= idx {
		return nil, fmt.Errorf("expected more than %d keymanager, have: %v", idx, kmLen)
	}
	km := kms[idx]

	ctrl, err := oasis.NewController(km.SocketPath())
	if err != nil {
		return nil, err
	}

	// Extract ExtraInfo.
	node, err := ctrl.Registry.GetNode(
		ctx,
		&registry.IDQuery{
			ID: km.NodeID,
		},
	)
	if err != nil {
		return nil, err
	}
	rt := node.GetRuntime(KeyManagerRuntimeID, version.Version{})
	if rt == nil {
		return nil, fmt.Errorf("key manager is missing keymanager runtime from descriptor")
	}
	var signedInitResponse keymanager.SignedInitResponse
	if err = cbor.Unmarshal(rt.ExtraInfo, &signedInitResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal extrainfo")
	}

	return &signedInitResponse.InitResponse, nil
}

// UpdateEnclavePolicies updates enclave policies with a new runtime deployment.
func (sc *Scenario) UpdateEnclavePolicies(rt *oasis.Runtime, deploymentIndex int, policies map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX) {
	enclaveID := rt.GetEnclaveIdentity(deploymentIndex)
	if enclaveID == nil {
		return
	}

	switch rt.Kind() {
	case registry.KindKeyManager:
		// Allow key manager runtime to replicate from all existing key managers.
		for _, policy := range policies {
			policy.MayReplicate = append(policy.MayReplicate, *enclaveID)
		}

		// Allow all runtimes to query the new key manager runtime.
		newPolicy := keymanager.EnclavePolicySGX{
			MayQuery:     make(map[common.Namespace][]sgx.EnclaveIdentity),
			MayReplicate: make([]sgx.EnclaveIdentity, 0),
		}
		for _, policy := range policies {
			for rt, enclaves := range policy.MayQuery {
				// Allowing duplicates, not important.
				newPolicy.MayQuery[rt] = append(newPolicy.MayQuery[rt], enclaves...)
			}
		}

		policies[*enclaveID] = &newPolicy
	case registry.KindCompute:
		// Allow compute runtime to query all existing key managers.
		for _, policy := range policies {
			policy.MayQuery[rt.ID()] = append(policy.MayQuery[rt.ID()], *enclaveID)
		}
	default:
		// Skip other kinds.
	}
}

// BuildAllEnclavePolicies builds enclave policies for all key manager runtimes.
//
// Policies are built from the fixture and adhere to the following rules:
//   - Each SGX runtime must have only one deployment and a distinct enclave identity.
//   - Key manager enclaves are not allowed to replicate the master secrets.
//   - All compute runtime enclaves are allowed to query key manager enclaves.
func (sc *Scenario) BuildAllEnclavePolicies(childEnv *env.Env) (map[common.Namespace]map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX, error) {
	sc.Logger.Info("building key manager SGX policy enclave policies map")

	kmPolicies := make(map[common.Namespace]map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX)

	// Each SGX runtime must have only one deployment.
	for _, rt := range sc.Net.Runtimes() {
		if len(rt.ToRuntimeDescriptor().Deployments) != 1 {
			return nil, fmt.Errorf("runtime should have only one deployment")
		}
	}

	// Each SGX runtime must have a distinct enclave identity.
	enclaveIDs := make(map[string]struct{})
	for _, rt := range sc.Net.Runtimes() {
		enclaveID := rt.GetEnclaveIdentity(0)
		if enclaveID == nil {
			continue
		}
		enclaveIDText, err := enclaveID.MarshalText()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal enclave identity: %w", err)
		}
		if _, ok := enclaveIDs[string(enclaveIDText)]; ok {
			return nil, fmt.Errorf("enclave identities are not unique")
		}
		enclaveIDs[string(enclaveIDText)] = struct{}{}
	}

	// Prepare empty policies for all key managers.
	for _, rt := range sc.Net.Runtimes() {
		if rt.Kind() != registry.KindKeyManager {
			continue
		}

		enclaveID := rt.GetEnclaveIdentity(0)
		if enclaveID == nil {
			continue
		}

		if _, ok := kmPolicies[rt.ID()]; ok {
			return nil, fmt.Errorf("duplicate key manager runtime: %s", rt.ID())
		}

		kmPolicies[rt.ID()] = map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX{
			*enclaveID: {
				MayQuery:     make(map[common.Namespace][]sgx.EnclaveIdentity),
				MayReplicate: make([]sgx.EnclaveIdentity, 0),
			},
		}
	}

	// Allow all compute runtime enclaves to query key manager enclave.
	for _, rt := range sc.Net.Runtimes() {
		if rt.Kind() != registry.KindCompute {
			continue
		}

		enclaveID := rt.GetEnclaveIdentity(0)
		if enclaveID == nil {
			continue
		}

		// Skip if the key manager runtime is not available.
		kmRtID := rt.ToRuntimeDescriptor().KeyManager
		policies, ok := kmPolicies[*kmRtID]
		if !ok {
			continue
		}

		for _, policy := range policies {
			policy.MayQuery[rt.ID()] = append(policy.MayQuery[rt.ID()], *enclaveID)
		}
	}

	return kmPolicies, nil
}

// BuildEnclavePolicies builds enclave policies for the simple key manager runtime.
//
// If the simple key manager runtime does not exist or is not running on an SGX platform,
// it returns nil.
func (sc *Scenario) BuildEnclavePolicies(childEnv *env.Env) (map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX, error) {
	policies, err := sc.BuildAllEnclavePolicies(childEnv)
	if err != nil {
		return nil, err
	}
	return policies[KeyManagerRuntimeID], nil
}

// ApplyKeyManagerPolicy applies the given policy to the simple key manager runtime.
func (sc *Scenario) ApplyKeyManagerPolicy(ctx context.Context, childEnv *env.Env, cli *cli.Helpers, rotationInterval beacon.EpochTime, policies map[sgx.EnclaveIdentity]*keymanager.EnclavePolicySGX, nonce uint64) error {
	status, err := sc.KeyManagerStatus(ctx)
	if err != nil && err != keymanager.ErrNoSuchStatus {
		return err
	}

	serial := uint32(1)
	if status != nil && status.Policy != nil {
		serial = status.Policy.Policy.Serial + 1
	}

	dir := childEnv.Dir()
	policyPath := filepath.Join(dir, "km_policy.cbor")
	sig1Path := filepath.Join(dir, "km_policy_sig1.pem")
	sig2Path := filepath.Join(dir, "km_policy_sig2.pem")
	sig3Path := filepath.Join(dir, "km_policy_sig3.pem")
	txPath := filepath.Join(dir, "km_gen_update.json")

	sc.Logger.Info("generating key manager policy")
	if err := cli.Keymanager.InitPolicy(KeyManagerRuntimeID, serial, rotationInterval, policies, policyPath); err != nil {
		return err
	}
	sc.Logger.Info("signing key manager policy")
	if err := cli.Keymanager.SignPolicy("1", policyPath, sig1Path); err != nil {
		return err
	}
	if err := cli.Keymanager.SignPolicy("2", policyPath, sig2Path); err != nil {
		return err
	}
	if err := cli.Keymanager.SignPolicy("3", policyPath, sig3Path); err != nil {
		return err
	}

	sc.Logger.Info("updating key manager policy")
	if err := cli.Keymanager.GenUpdate(nonce, policyPath, []string{sig1Path, sig2Path, sig3Path}, txPath); err != nil {
		return err
	}
	if err := cli.Consensus.SubmitTx(txPath); err != nil {
		return fmt.Errorf("failed to update key manager policy: %w", err)
	}

	return nil
}
