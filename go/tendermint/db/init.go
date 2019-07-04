// Package db implements several Tendermint DB backends.
package db

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/node"

	"github.com/oasislabs/ekiden/go/tendermint/db/badger"
	"github.com/oasislabs/ekiden/go/tendermint/db/bolt"
)

const cfgBackend = "tendermint.db.backend"

// GetProvider returns the currently configured Tendermint DBProvider.
func GetProvider() (node.DBProvider, error) {
	backend := viper.GetString(cfgBackend)

	switch strings.ToLower(backend) {
	case badger.BackendName:
		return badger.DBProvider, nil
	case bolt.BackendName:
		return bolt.DBProvider, nil
	default:
		return nil, fmt.Errorf("tendermint/db: unsupported backend: '%v'", backend)
	}
}

// New constructs a new tendermint DB with the configured backend.
func New(fn string, noSuffix bool) (dbm.DB, error) {
	backend := viper.GetString(cfgBackend)

	switch strings.ToLower(backend) {
	case badger.BackendName:
		return badger.New(fn, noSuffix)
	case bolt.BackendName:
		return bolt.New(fn, noSuffix)
	default:
		return nil, fmt.Errorf("tendermint/db: unsupported backend: '%v'", backend)
	}
}

// RegisterFlags registers the configuration flags with the provided command.
func RegisterFlags(cmd *cobra.Command) {
	if !cmd.Flags().Parsed() {
		cmd.Flags().String(cfgBackend, bolt.BackendName, "tendermint db backend")
	}

	for _, v := range []string{
		cfgBackend,
	} {
		_ = viper.BindPFlag(v, cmd.Flags().Lookup(v))
	}
}