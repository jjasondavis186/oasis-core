package commitment

import (
	"context"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/hash"
	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/errors"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	p2pError "github.com/oasisprotocol/oasis-core/go/p2p/error"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/message"
	scheduler "github.com/oasisprotocol/oasis-core/go/scheduler/api"
)

// moduleName is the module name used for namespacing errors.
const moduleName = "roothash/commitment"

// nolint: revive
var (
	ErrNoRuntime              = errors.New(moduleName, 1, "roothash/commitment: no runtime configured")
	ErrNoCommittee            = errors.New(moduleName, 2, "roothash/commitment: no committee configured")
	ErrInvalidCommitteeKind   = errors.New(moduleName, 3, "roothash/commitment: invalid committee kind")
	ErrRakSigInvalid          = errors.New(moduleName, 4, "roothash/commitment: batch RAK signature invalid")
	ErrNotInCommittee         = errors.New(moduleName, 5, "roothash/commitment: node not part of committee")
	ErrAlreadyCommitted       = errors.New(moduleName, 6, "roothash/commitment: node already sent commitment")
	ErrNotBasedOnCorrectBlock = errors.New(moduleName, 7, "roothash/commitment: submitted commitment is not based on correct block")
	ErrDiscrepancyDetected    = errors.New(moduleName, 8, "roothash/commitment: discrepancy detected")
	ErrStillWaiting           = errors.New(moduleName, 9, "roothash/commitment: still waiting for commits")
	ErrInsufficientVotes      = errors.New(moduleName, 10, "roothash/commitment: insufficient votes to finalize discrepancy resolution round")
	ErrBadExecutorCommitment  = errors.New(moduleName, 11, "roothash/commitment: bad executor commitment")
	// Error code 12 is reserved for future use.
	ErrInvalidMessages = p2pError.Permanent(errors.New(moduleName, 13, "roothash/commitment: invalid messages"))
	// Error code 14 is reserved for future use.
	ErrTimeoutNotCorrectRound = errors.New(moduleName, 15, "roothash/commitment: timeout not for correct round")
	ErrNodeIsScheduler        = errors.New(moduleName, 16, "roothash/commitment: node is scheduler")
	ErrInvalidRound           = errors.New(moduleName, 17, "roothash/commitment: invalid round")
	ErrNoProposerCommitment   = errors.New(moduleName, 18, "roothash/commitment: no proposer commitment")
	ErrBadProposerCommitment  = errors.New(moduleName, 19, "roothash/commitment: bad proposer commitment")
)

const (
	// TimeoutNever is the timeout value that never expires.
	TimeoutNever = 0

	// Backup worker round timeout stretch factor (15/10 = 1.5).
	backupWorkerTimeoutFactorNumerator   = 15
	backupWorkerTimeoutFactorDenominator = 10

	// LogEventDiscrepancyMajorityFailure is a log event value that dependency resoluton with majority failure.
	LogEventDiscrepancyMajorityFailure = "pool/discrepancy_majority_failure"
)

var logger *logging.Logger = logging.GetLogger("roothash/commitment/pool")

// NodeLookup is an interface for looking up registry node descriptors.
type NodeLookup interface {
	// Node looks up a node descriptor.
	Node(ctx context.Context, id signature.PublicKey) (*node.Node, error)
}

// MessageValidator is an arbitrary function that validates messages for validity. It can be used
// for gas accounting.
type MessageValidator func(msgs []message.Message) error

// Pool is a serializable pool of commitments that can be used to perform
// discrepancy detection.
//
// The pool is not safe for concurrent use.
type Pool struct {
	// Runtime is the runtime descriptor this pool is collecting the
	// commitments for.
	Runtime *registry.Runtime `json:"runtime"`
	// Committee is the committee this pool is collecting the commitments for.
	Committee *scheduler.Committee `json:"committee"`
	// Round is the current protocol round.
	Round uint64 `json:"round"`
	// ExecuteCommitments are the commitments in the pool iff Committee.Kind
	// is scheduler.KindComputeExecutor.
	ExecuteCommitments map[signature.PublicKey]*ExecutorCommitment `json:"execute_commitments,omitempty"`
	// Discrepancy is a flag signalling that a discrepancy has been detected.
	Discrepancy bool `json:"discrepancy"`
	// NextTimeout is the time when the next call to TryFinalize(true) should
	// be scheduled to be executed. Zero means that no timeout is to be scheduled.
	NextTimeout int64 `json:"next_timeout"`

	// memberSet is a cached committee member set. It will be automatically
	// constructed based on the passed Committee.
	memberSet map[signature.PublicKey]bool

	// workerSet is a cached committee worker set. It will be automatically
	// constructed based on the passed Committee.
	workerSet map[signature.PublicKey]bool
}

func (p *Pool) computeMemberSets() {
	if p.Committee == nil {
		return
	}

	p.memberSet = make(map[signature.PublicKey]bool, len(p.Committee.Members))
	p.workerSet = make(map[signature.PublicKey]bool)
	for _, m := range p.Committee.Members {
		p.memberSet[m.PublicKey] = true
		if m.Role == scheduler.RoleWorker {
			p.workerSet[m.PublicKey] = true
		}
	}
}

func (p *Pool) isMember(id signature.PublicKey) bool {
	if p.Committee == nil {
		return false
	}

	if len(p.memberSet) == 0 {
		p.computeMemberSets()
	}

	return p.memberSet[id]
}

func (p *Pool) isWorker(id signature.PublicKey) bool {
	if p.Committee == nil {
		return false
	}

	if len(p.workerSet) == 0 {
		p.computeMemberSets()
	}

	return p.workerSet[id]
}

func (p *Pool) isScheduler(id signature.PublicKey) bool {
	if p.Committee == nil {
		return false
	}
	scheduler, err := p.Committee.TransactionScheduler(p.Round)
	if err != nil {
		return false
	}

	return scheduler.PublicKey.Equal(id)
}

// ResetCommitments resets the commitments in the pool, clears the discrepancy flag and the next
// timeout height.
func (p *Pool) ResetCommitments(round uint64) {
	p.Round = round
	if p.ExecuteCommitments == nil || len(p.ExecuteCommitments) > 0 {
		p.ExecuteCommitments = make(map[signature.PublicKey]*ExecutorCommitment)
	}
	p.Discrepancy = false
	p.NextTimeout = TimeoutNever
}

func (p *Pool) addVerifiedExecutorCommitment( // nolint: gocyclo
	ctx context.Context,
	blk *block.Block,
	nl NodeLookup,
	msgValidator MessageValidator,
	commit *ExecutorCommitment,
) error {
	if p.Committee == nil {
		return ErrNoCommittee
	}
	if p.Committee.Kind != scheduler.KindComputeExecutor {
		return ErrInvalidCommitteeKind
	}

	// Ensure that the node is actually a committee member. We do not enforce specific
	// roles based on current discrepancy state to allow commitments arriving in any
	// order (e.g., a backup worker can submit a commitment even before there is a
	// discrepancy).
	if !p.isMember(commit.NodeID) {
		return ErrNotInCommittee
	}

	// Ensure the node did not already submit a commitment.
	if _, ok := p.ExecuteCommitments[commit.NodeID]; ok {
		return ErrAlreadyCommitted
	}

	if p.Runtime == nil {
		return ErrNoRuntime
	}
	if p.Round != blk.Header.Round {
		logger.Error("incorrectly configured pool",
			"round", p.Round,
			"blk_round", blk.Header.Round,
		)
		return ErrInvalidRound
	}

	// Check if the block is based on the previous block.
	if !commit.Header.Header.IsParentOf(&blk.Header) {
		logger.Debug("executor commitment is not based on correct block",
			"node_id", commit.NodeID,
			"expected_previous_hash", blk.Header.EncodedHash(),
			"previous_hash", commit.Header.Header.PreviousHash,
		)
		return ErrNotBasedOnCorrectBlock
	}

	if err := commit.ValidateBasic(); err != nil {
		logger.Debug("executor commitment validate basic error",
			"err", err,
		)
		return ErrBadExecutorCommitment
	}

	// TODO: Check for evidence of equivocation (oasis-core#3685).

	switch commit.IsIndicatingFailure() {
	case true:
	default:
		// Verify RAK-attestation.
		if p.Runtime.TEEHardware != node.TEEHardwareInvalid {
			n, err := nl.Node(ctx, commit.NodeID)
			if err != nil {
				// This should never happen as nodes cannot disappear mid-epoch.
				logger.Warn("unable to fetch node descriptor to verify RAK-attestation",
					"err", err,
					"node_id", commit.NodeID,
				)
				return ErrNotInCommittee
			}

			ad := p.Runtime.ActiveDeployment(p.Committee.ValidFor)
			if ad == nil {
				// This should never happen as we prevent this elsewhere.
				logger.Error("no active deployment",
					"runtime_id", p.Runtime.ID,
					"node_id", commit.NodeID,
					"deployments", p.Runtime.Deployments,
				)
				return ErrNoRuntime
			}

			rt := n.GetRuntime(p.Runtime.ID, ad.Version)
			if rt == nil {
				// We currently prevent this case throughout the rest of the system.
				// Still, it's prudent to check.
				logger.Warn("committee member not registered with this runtime",
					"runtime_id", p.Runtime.ID,
					"node_id", commit.NodeID,
				)
				return ErrNotInCommittee
			}

			if rt.Capabilities.TEE == nil {
				// This should never happen as we prevent this elsewhere.
				logger.Error("node doesn't have TEE capability",
					"runtime_id", p.Runtime.ID,
					"node_id", commit.NodeID,
				)
				return ErrRakSigInvalid
			}

			if err = commit.Header.VerifyRAK(rt.Capabilities.TEE.RAK); err != nil {
				return ErrRakSigInvalid
			}
		}

		// Check emitted runtime messages.
		switch p.isScheduler(commit.NodeID) {
		case true:
			// The transaction scheduler can include messages.
			if uint32(len(commit.Messages)) > p.Runtime.Executor.MaxMessages {
				logger.Debug("executor commitment from scheduler has too many messages",
					"node_id", commit.NodeID,
					"num_messages", len(commit.Messages),
					"max_messages", p.Runtime.Executor.MaxMessages,
				)
				return ErrInvalidMessages
			}
			if h := message.MessagesHash(commit.Messages); !h.Equal(commit.Header.Header.MessagesHash) {
				logger.Debug("executor commitment from scheduler has invalid messages hash",
					"node_id", commit.NodeID,
					"expected_hash", h,
					"messages_hash", commit.Header.Header.MessagesHash,
				)
				return ErrInvalidMessages
			}

			// Perform custom message validation and propagate the error unchanged.
			if msgValidator != nil && len(commit.Messages) > 0 {
				err := msgValidator(commit.Messages)
				if err != nil {
					logger.Debug("executor commitment from scheduler has invalid messages",
						"err", err,
						"node_id", commit.NodeID,
					)
					return err
				}
			}
		case false:
			// Other workers cannot include any messages.
			if len(commit.Messages) > 0 {
				logger.Debug("executor commitment from non-scheduler contains messages",
					"node_id", commit.NodeID,
					"num_messages", len(commit.Messages),
				)
				return ErrInvalidMessages
			}
		}
	}

	if p.ExecuteCommitments == nil {
		p.ExecuteCommitments = make(map[signature.PublicKey]*ExecutorCommitment)
	}
	p.ExecuteCommitments[commit.NodeID] = commit

	return nil
}

// AddExecutorCommitment verifies and adds a new executor commitment to the pool.
func (p *Pool) AddExecutorCommitment(
	ctx context.Context,
	blk *block.Block,
	nl NodeLookup,
	commit *ExecutorCommitment,
	msgValidator MessageValidator,
) error {
	if p.Runtime == nil {
		return ErrNoRuntime
	}

	// Check executor commitment signature.
	if err := commit.Verify(p.Runtime.ID); err != nil {
		return p2pError.Permanent(err)
	}

	return p.addVerifiedExecutorCommitment(ctx, blk, nl, msgValidator, commit)
}

// ProcessCommitments performs a single round of commitment checks. If there are enough commitments
// in the pool, it performs discrepancy detection or resolution.
func (p *Pool) ProcessCommitments(didTimeout bool) (OpenCommitment, error) {
	switch {
	case p.Committee == nil:
		return nil, ErrNoCommittee
	case p.Committee.Kind != scheduler.KindComputeExecutor:
		panic("roothash/commitment: unknown committee kind: " + p.Committee.Kind.String())
	}

	type vote struct {
		commit OpenCommitment
		tally  int
	}

	var total, commits, failures int

	// Gather votes.
	votes := make(map[hash.Hash]*vote)
	for _, n := range p.Committee.Members {
		switch {
		case !p.Discrepancy && n.Role != scheduler.RoleWorker:
			continue
		case p.Discrepancy && n.Role != scheduler.RoleBackupWorker:
			continue
		}

		total++
		commit, ok := p.ExecuteCommitments[n.PublicKey]
		if !ok {
			continue
		}
		commits++

		if commit.IsIndicatingFailure() {
			failures++
			continue
		}

		k := commit.ToVote()
		if v, ok := votes[k]; !ok {
			votes[k] = &vote{
				commit: commit,
				tally:  1,
			}
		} else {
			v.tally++
		}

		// As soon as there is a discrepancy we can proceed with discrepancy resolution.
		// No need to wait for all commits.
		if !p.Discrepancy && len(votes) > 1 {
			p.Discrepancy = true
			return nil, ErrDiscrepancyDetected
		}
	}

	// Determine whether the proposer has submitted a commitment.
	proposer, err := p.Committee.TransactionScheduler(p.Round)
	if err != nil {
		return nil, ErrNoCommittee
	}
	proposerCommit, ok := p.ExecuteCommitments[proposer.PublicKey]
	if !ok && didTimeout {
		// TODO: Consider slashing for this offense.
		return nil, ErrNoProposerCommitment
	}

	switch p.Discrepancy {
	case false:
		// Discrepancy detection.
		allowedStragglers := int(p.Runtime.Executor.AllowedStragglers)

		// If it is already known that the number of valid commitments will not exceed the required
		// threshold, there is no need to wait for the timer to expire. Instead, proceed directly to
		// the discrepancy resolution mode, regardless of any additional commits.
		if failures > allowedStragglers {
			p.Discrepancy = true
			return nil, ErrDiscrepancyDetected
		}

		// While a timer is running, all nodes are required to answer.
		required := total

		// After the timeout has elapsed, a limited number of stragglers are allowed.
		if didTimeout {
			required -= allowedStragglers
			commits -= failures // Since failures count as stragglers.
		}

		// Check if the majority has been reached.
		if commits < required || proposerCommit == nil {
			return nil, ErrStillWaiting
		}

	case true:
		// Discrepancy resolution.
		required := total/2 + 1

		// Find the commit with the highest number of votes.
		topVote := &vote{}
		for _, v := range votes {
			if v.tally > topVote.tally {
				topVote = v
			}
		}

		// Fail the round if the majority cannot be reached due to insufficient votes remaining
		// (e.g. too many nodes have failed),
		remaining := total - commits
		if topVote.tally+remaining < required {
			return nil, ErrInsufficientVotes
		}

		// Check if the majority has been reached.
		if topVote.tally < required || proposerCommit == nil {
			if didTimeout {
				return nil, ErrInsufficientVotes
			}
			return nil, ErrStillWaiting
		}

		// Make sure that the majority commitment is the same as the proposer commitment.
		if !proposerCommit.MostlyEqual(topVote.commit) {
			return nil, ErrBadProposerCommitment
		}
	}

	// We must return the proposer commitment as that one contains additional data.
	return proposerCommit, nil
}

// CheckProposerTimeout verifies executor timeout request conditions.
func (p *Pool) CheckProposerTimeout(
	ctx context.Context,
	block *block.Block,
	nl NodeLookup,
	id signature.PublicKey,
	round uint64,
) error {
	if p.Committee == nil {
		return ErrNoCommittee
	}
	if p.Committee.Kind != scheduler.KindComputeExecutor {
		return ErrInvalidCommitteeKind
	}

	// Ensure timeout is for correct round.
	if round != block.Header.Round {
		return ErrTimeoutNotCorrectRound
	}

	// Ensure there is no commitments yet.
	if len(p.ExecuteCommitments) != 0 {
		return ErrAlreadyCommitted
	}

	// Ensure that the node that is requesting a timeout is actually a committee
	// worker.
	if !p.isWorker(id) {
		return ErrNotInCommittee
	}

	// Ensure that the node requesting a timeout is not the scheduler for
	// current round.
	if p.isScheduler(id) {
		return ErrNodeIsScheduler
	}

	return nil
}

// TryFinalize attempts to finalize the commitments by performing discrepancy
// detection and discrepancy resolution, based on the state of the pool. It may
// request the caller to schedule timeouts by setting NextTimeout appropriately.
//
// If a timeout occurs and isTimeoutAuthoritative is false, the internal
// discrepancy flag will not be changed but the method will still return the
// ErrDiscrepancyDetected error.
func (p *Pool) TryFinalize(
	height int64,
	roundTimeout int64,
	didTimeout bool,
	isTimeoutAuthoritative bool,
) (OpenCommitment, error) {
	var rearmTimer bool
	defer func() {
		if rearmTimer {
			p.NextTimeout = height + roundTimeout
		} else {
			p.NextTimeout = TimeoutNever
		}
	}()

	switch commit, err := p.ProcessCommitments(didTimeout); err {
	case nil:
		return commit, nil
	case ErrStillWaiting:
		if didTimeout {
			// This is the fast path and the round timer expired.
			//
			// Transition to the discrepancy state so the backup workers
			// process the round, assuming that it is possible to do so.
			if isTimeoutAuthoritative {
				p.Discrepancy = true
				// Arm the timer, but increase the roundTimeout as the backup workers should be
				// given some more time to do the computation.
				rearmTimer = true
				roundTimeout = (backupWorkerTimeoutFactorNumerator * roundTimeout) / backupWorkerTimeoutFactorDenominator
			}
			return nil, ErrDiscrepancyDetected
		}

		// Insufficient commitments for finalization, wait.
		rearmTimer = true
		return nil, err
	case ErrDiscrepancyDetected:
		rearmTimer = true
		return nil, err
	default:
		return nil, err
	}
}

// IsTimeout returns true if the time is up for pool's TryFinalize to be called.
func (p *Pool) IsTimeout(height int64) bool {
	return p.NextTimeout != TimeoutNever && height >= p.NextTimeout
}
