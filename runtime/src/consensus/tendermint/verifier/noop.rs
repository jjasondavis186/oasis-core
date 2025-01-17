use std::sync::Arc;

use anyhow::anyhow;
use async_trait::async_trait;
use slog::info;

use crate::{
    common::logger::get_logger,
    consensus::{
        beacon::EpochTime,
        roothash::Header,
        state::ConsensusState,
        tendermint::decode_light_block,
        verifier::{self, Error},
        Event, LightBlock, HEIGHT_LATEST,
    },
    protocol::Protocol,
    types::{Body, EventKind, HostFetchConsensusEventsRequest, HostFetchConsensusEventsResponse},
};

/// A verifier which performs no verification.
pub struct NopVerifier {
    protocol: Arc<Protocol>,
}

impl NopVerifier {
    /// Create a new non-verifying verifier.
    pub fn new(protocol: Arc<Protocol>) -> Self {
        Self { protocol }
    }

    /// Start the non-verifying verifier.
    pub fn start(&self) {
        let logger = get_logger("consensus/cometbft/verifier");
        info!(logger, "Starting consensus noop verifier");
    }

    async fn fetch_light_block(&self, height: u64) -> Result<LightBlock, Error> {
        let result = self
            .protocol
            .call_host_async(Body::HostFetchConsensusBlockRequest { height })
            .await
            .map_err(|err| Error::VerificationFailed(err.into()))?;

        match result {
            Body::HostFetchConsensusBlockResponse { block } => Ok(block),
            _ => Err(Error::VerificationFailed(anyhow!("bad response from host"))),
        }
    }
}

#[async_trait]
impl verifier::Verifier for NopVerifier {
    async fn sync(&self, _height: u64) -> Result<(), Error> {
        Ok(())
    }

    async fn verify(
        &self,
        consensus_block: LightBlock,
        _runtime_header: Header,
        _epoch: EpochTime,
    ) -> Result<ConsensusState, Error> {
        self.unverified_state(consensus_block).await
    }

    async fn verify_for_query(
        &self,
        consensus_block: LightBlock,
        _runtime_header: Header,
        _epoch: EpochTime,
    ) -> Result<ConsensusState, Error> {
        self.unverified_state(consensus_block).await
    }

    async fn unverified_state(&self, consensus_block: LightBlock) -> Result<ConsensusState, Error> {
        let untrusted_block =
            decode_light_block(consensus_block).map_err(Error::VerificationFailed)?;
        // NOTE: No actual verification is performed.
        let state_root = untrusted_block.get_state_root();
        Ok(ConsensusState::from_protocol(
            self.protocol.clone(),
            state_root.version + 1,
            state_root,
        ))
    }

    async fn latest_state(&self) -> Result<ConsensusState, Error> {
        self.state_at(HEIGHT_LATEST).await
    }

    async fn state_at(&self, height: u64) -> Result<ConsensusState, Error> {
        let block = self.fetch_light_block(height).await?;
        self.unverified_state(block).await
    }

    async fn events_at(&self, height: u64, kind: EventKind) -> Result<Vec<Event>, Error> {
        let result = self
            .protocol
            .call_host_async(Body::HostFetchConsensusEventsRequest(
                HostFetchConsensusEventsRequest { height, kind },
            ))
            .await
            .map_err(|err| Error::VerificationFailed(err.into()))?;

        match result {
            Body::HostFetchConsensusEventsResponse(HostFetchConsensusEventsResponse { events }) => {
                Ok(events)
            }
            _ => Err(Error::VerificationFailed(anyhow!("bad response from host"))),
        }
    }

    async fn latest_height(&self) -> Result<u64, Error> {
        Ok(self.fetch_light_block(HEIGHT_LATEST).await?.height)
    }
}
