// Package api provides a light CometBFT consensus backend API.
package api

import (
	"context"

	cmtlight "github.com/cometbft/cometbft/light"
	cmtlightprovider "github.com/cometbft/cometbft/light/provider"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	cmttypes "github.com/cometbft/cometbft/types"

	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
)

// ClientService is a CometBFT consensus light client service.
type ClientService interface {
	consensus.LightService

	Client
}

// Client is a CometBFT consensus light client that talks with remote oasis-nodes that are using
// the CometBFT consensus backend and verifies responses.
type Client interface {
	consensus.LightClient

	// GetVerifiedLightBlock returns a verified light block.
	GetVerifiedLightBlock(ctx context.Context, height int64) (*cmttypes.LightBlock, error)

	// GetVerifiedParameters returns verified consensus parameters.
	GetVerifiedParameters(ctx context.Context, height int64) (*cmtproto.ConsensusParams, error)
}

// Provider is a CometBFT light client provider.
type Provider interface {
	cmtlightprovider.Provider
	consensus.LightClient

	// Initialized returns a channel that is closed when the provider is initialized.
	Initialized() <-chan struct{}

	// PeerID returns the identifier of the peer backing the provider.
	PeerID() string
}

// ClientConfig is the configuration for the light client.
type ClientConfig struct {
	// GenesisDocument is the CometBFT genesis document.
	GenesisDocument *cmttypes.GenesisDoc

	// TrustOptions are CometBFT light client trust options.
	TrustOptions cmtlight.TrustOptions
}
