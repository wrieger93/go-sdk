package raft

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blend/go-sdk/exception"

	"github.com/blend/go-sdk/logger"
	"github.com/blend/go-sdk/uuid"
	"github.com/blend/go-sdk/worker"
)

const (
	// ErrAlreadyStarted is returned if you call start on a started node.
	ErrAlreadyStarted = Error("raft is already started")
	// ErrNotRunning is returned if you try and call stop on a stopped node.
	ErrNotRunning = Error("raft is not running")
)

// New creates a new empty raft node.
func New() *Raft {
	return &Raft{
		id:                  uuid.V4().String(),
		state:               Follower,
		bindAddr:            DefaultBindAddr,
		latch:               &worker.Latch{},
		electionTimeout:     DefaultElectionTimeout,
		leaderCheckInterval: DefaultLeaderCheckInterval,
		heartbeatInterval:   DefaultHeartbeatInterval,
	}
}

// NewFromConfig creates a new raft node from a config.
func NewFromConfig(cfg *Config) *Raft {
	return New().
		WithID(cfg.GetID()).
		WithBindAddr(cfg.GetBindAddr()).
		WithSelfAddr(cfg.GetSelfAddr()).
		WithHeartbeatInterval(cfg.GetHeartbeatInterval()).
		WithLeaderCheckInterval(cfg.GetLeaderCheckInterval()).
		WithElectionTimeout(cfg.GetElectionTimeout())
}

// Raft represents a raft node and all the state machine
// componentry required.
type Raft struct {
	sync.Mutex

	id       string
	log      *logger.Logger
	selfAddr string
	bindAddr string

	electionTimeout time.Duration

	leaderCheckInterval time.Duration
	heartbeatInterval   time.Duration

	// raft state fields

	// currentTerm is the current election term. starts at zero.
	// incremented every election() and by append entries from new leaders.
	currentTerm       uint64
	votedFor          string
	lastLeaderContact time.Time
	lastVoteGranted   time.Time

	// state is the current fsm state
	state        State
	backoffIndex int32

	server Server
	peers  []Client

	latch             *worker.Latch
	leaderCheckTicker *worker.Interval
	heartbeatTicker   *worker.Interval

	leaderHandler    func()
	candidateHandler func()
	followerHandler  func()
}

// Start starts the raft node.
func (r *Raft) Start() error {
	r.Lock()
	defer r.Unlock()

	if r.latch.IsStarting() || r.latch.IsRunning() {
		return exception.New(ErrAlreadyStarted)
	}
	r.latch.Starting()

	r.infof("node starting")
	defer func() {
		r.infof("node started")
	}()

	if len(r.peers) == 0 {
		r.infof("node operating in solo node configuration")
		r.transitionTo(Leader)
		return nil
	}

	if r.server == nil {
		r.server = NewRPCServer().WithBindAddr(r.BindAddr()).WithLogger(r.log)
	}

	// wire up the rpc server.
	r.server.SetAppendEntriesHandler(r.AppendEntriesHandler)
	r.server.SetRequestVoteHandler(r.RequestVoteHandler)

	r.infof("node rpc server starting, listening on: %s", r.BindAddr())
	err := r.server.Start()
	if err != nil {
		return err
	}

	r.leaderCheckTicker = worker.NewInterval(r.LeaderCheck, r.leaderCheckInterval)
	r.leaderCheckTicker.Start()

	r.heartbeatTicker = worker.NewInterval(r.Heartbeat, r.heartbeatInterval)
	r.heartbeatTicker.Start()

	r.latch.Started()
	return nil
}

// Stop stops the node.
func (r *Raft) Stop() error {
	r.Lock()
	defer r.Unlock()

	if !r.latch.IsRunning() {
		return exception.New(ErrNotRunning)
	}
	if r.leaderCheckTicker != nil {
		r.leaderCheckTicker.Stop()
		r.leaderCheckTicker = nil
	}
	if r.heartbeatTicker != nil {
		r.heartbeatTicker.Stop()
		r.heartbeatTicker = nil
	}

	if r.server != nil {
		return r.server.Stop()
	}
	return nil
}

// LeaderCheck is the action that fires on an interval to check if the leader lease has expired.
// If it fails, it triggers an election.
func (r *Raft) LeaderCheck() error {
	if r.getStateSafe() == Follower {
		// if we've never elected a leader, or if the current leader hasn't sent a heartbeat in a while ...
		if r.isLeaderFailed() {
			// trigger an election.
			r.err(r.election())
		}
	}
	return nil
}

// Heartbeat is the action triggered upon send heartbeat.
// This method is fully interlocked.
// This method launches a goroutine.
func (r *Raft) Heartbeat() error {
	if r.getStateSafe() != Leader {
		return nil
	}
	r.sendHeartbeat()
	return nil
}

// AppendEntriesHandler is the rpc server handler for AppendEntries rpc requests.
// This method is fully interlocked.
func (r *Raft) AppendEntriesHandler(args *AppendEntries, res *AppendEntriesResults) error {
	r.Lock()
	defer r.Unlock()

	if args.Term < r.currentTerm {
		r.debugf("received out of date leader heartbeat (%d vs. %d)", args.Term, r.currentTerm)
		*res = AppendEntriesResults{
			ID:      r.id,
			Success: false,
			Term:    r.currentTerm,
		}
		return nil
	}

	//r.debugf("received leader heartbeat from %s", args.ID)
	r.transitionTo(Follower)
	r.currentTerm = args.Term
	r.lastLeaderContact = time.Now().UTC()

	*res = AppendEntriesResults{
		ID:      r.id,
		Success: true,
		Term:    r.currentTerm,
	}
	return nil
}

// RequestVoteHandler is the rpc server handler for RequestVote rpc requests.
// This method is fully interlocked.
func (r *Raft) RequestVoteHandler(args *RequestVote, res *RequestVoteResults) error {
	r.Lock()
	defer r.Unlock()

	if !r.lastVoteGranted.IsZero() && r.now().Sub(r.lastVoteGranted) > r.electionTimeout {
		r.debugf("resetting expired vote")
		r.votedFor = ""
	}

	if args.Term < r.currentTerm {
		r.debugf("rejecting request vote from %s @ %d", args.ID, args.Term)
		*res = RequestVoteResults{
			ID:      r.id,
			Term:    r.currentTerm,
			Granted: false,
		}
		return nil
	}

	if len(r.votedFor) > 0 && r.votedFor != args.ID {
		r.debugf("rejecting request vote from %s @ %d", args.ID, args.Term)
		*res = RequestVoteResults{
			ID:      r.id,
			Term:    r.currentTerm,
			Granted: false,
		}
	}

	r.debugf("accepting request vote from %s @ %d", args.ID, args.Term)
	r.transitionTo(Follower)
	r.votedFor = args.ID
	r.currentTerm = args.Term
	r.lastVoteGranted = time.Now().UTC()

	*res = RequestVoteResults{
		ID:      r.id,
		Term:    args.Term,
		Granted: true,
	}
	return nil
}

// --------------------------------------------------------------------------------
// helper methods
// --------------------------------------------------------------------------------

func (r *Raft) getStateSafe() (state State) {
	r.Lock()
	state = r.state
	r.Unlock()
	return
}

func (r *Raft) isLeaderFailed() (output bool) {
	r.Lock()
	now := time.Now().UTC()
	output = r.lastLeaderContact.IsZero() || now.Sub(r.lastLeaderContact) > RandomTimeout(r.electionTimeout)
	r.Unlock()
	return
}

// Election requests votes from all peers, totalling the results and potentially promoting self to leader.
// It is time bound on the ElectionTimeout.
func (r *Raft) election() error {
	r.debugf("election triggered")
	r.interlocked(func() {
		r.votedFor = r.ID()
		r.currentTerm = r.currentTerm + 1
		r.transitionTo(Candidate)
	})

	started := time.Now().UTC()
	for time.Since(started) < r.electionTimeout {

		if r.getStateSafe() != Candidate {
			return nil
		}

		if result, err := r.requestVotes(); err != nil {
			return err
		} else if result == ElectionVictory {
			r.debugf("election successful, promoting self to leader")
			r.interlocked(func() {
				r.transitionTo(Leader)
			})
			return r.Heartbeat() // send immediate heartbeat
		} else {
			r.debugf("election loss or tie, backing off")
			r.backoff(r.electionTimeout)
		}
	}

	r.debugf("election timed out")
	r.interlocked(func() {
		r.transitionTo(Follower)
		r.votedFor = ""
	})
	r.backoff(r.electionTimeout)
	return nil
}

// requestVotes sends `RequestVote` rpcs to all peers, and totals the results.
func (r *Raft) requestVotes() (result ElectionOutcome, err error) {
	voteRequest := RequestVote{
		ID:   r.id,
		Term: r.currentTerm,
	}

	results := make(chan *RequestVoteResults, len(r.peers))
	errs := make(chan error, len(r.peers))
	wg := sync.WaitGroup{}
	wg.Add(len(r.peers))

	for _, peer := range r.peers {
		go func(c Client) {
			defer wg.Done()

			res, err := c.RequestVote(&voteRequest)
			if err != nil {
				r.debugf("requesting vote from %s: error", c.RemoteAddr())
				errs <- err
			} else {
				r.debugf("requesting vote from %s: %v", c.RemoteAddr(), res.Granted)
				results <- res
			}
		}(peer)
	}
	wg.Wait()
	r.logErrors(errs)

	result = r.processRequestVoteResults(results)
	r.debugf("election result: %v", result)
	return
}

func (r *Raft) sendHeartbeat() {
	args := AppendEntries{
		ID:   r.id,
		Term: r.currentTerm,
	}

	results := make(chan *AppendEntriesResults, len(r.peers))
	errs := make(chan error, len(r.peers))

	wg := sync.WaitGroup{}
	wg.Add(len(r.peers))
	for _, peer := range r.peers {
		go func(c Client) {
			defer wg.Done()
			res, err := c.AppendEntries(&args)
			if err != nil {
				errs <- err
			} else {
				results <- res
			}
		}(peer)
	}
	wg.Wait()

	r.logErrors(errs)
}

// --------------------------------------------------------------------------------
// properties
// --------------------------------------------------------------------------------

// WithID sets the identifier for the node.
func (r *Raft) WithID(id string) *Raft {
	r.id = id
	return r
}

// ID is the raft node identifier.
func (r *Raft) ID() string {
	return r.id
}

// WithBindAddr sets the rpc server bind address.
func (r *Raft) WithBindAddr(bindAddr string) *Raft {
	r.bindAddr = bindAddr
	return r
}

// BindAddr returns the rpc server bind address.
func (r *Raft) BindAddr() string {
	return r.bindAddr
}

// WithSelfAddr sets the rpc server bind address.
func (r *Raft) WithSelfAddr(selfAddr string) *Raft {
	r.selfAddr = selfAddr
	return r
}

// SelfAddr returns the rpc server bind address.
func (r *Raft) SelfAddr() string {
	return r.selfAddr
}

// IsSelf returns if a remoteAddr match this node's address.
// If SelfAddr() is unset, this will return false.
func (r *Raft) IsSelf(remoteAddr string) bool {
	if len(r.selfAddr) == 0 {
		return false
	}
	return r.selfAddr == strings.TrimSpace(remoteAddr)
}

// State returns the current raft state. It is read only.
func (r *Raft) State() State {
	return r.state
}

// VotedFor returns the current known leader. It is read only.
func (r *Raft) VotedFor() string {
	return r.votedFor
}

// CurrentTerm returns the current raft term. It is read only.
func (r *Raft) CurrentTerm() uint64 {
	return r.currentTerm
}

// LastLeaderContact is the last time we heard from the leader. It is read only.
func (r *Raft) LastLeaderContact() time.Time {
	return r.lastLeaderContact
}

// SetLeaderHandler sets the leader handler.
func (r *Raft) SetLeaderHandler(handler func()) {
	r.leaderHandler = handler
}

// SetCandidateHandler sets the leader handler.
func (r *Raft) SetCandidateHandler(handler func()) {
	r.candidateHandler = handler
}

// SetFollowerHandler sets the leader handler.
func (r *Raft) SetFollowerHandler(handler func()) {
	r.followerHandler = handler
}

// WithLogger sets the logger.
func (r *Raft) WithLogger(log *logger.Logger) *Raft {
	r.log = log
	return r
}

// Logger returns the logger.
func (r *Raft) Logger() *logger.Logger {
	return r.log
}

// WithPeer adds a peer.
func (r *Raft) WithPeer(peer Client) *Raft {
	r.peers = append(r.peers, peer)
	return r
}

// Peers returns the raft peers.
func (r *Raft) Peers() []Client {
	return r.peers
}

// WithServer sets the rpc server.
func (r *Raft) WithServer(server Server) *Raft {
	r.server = server
	return r
}

// Server returns the rpc server.
func (r *Raft) Server() Server {
	return r.server
}

// WithElectionTimeout sets the election timeout.
func (r *Raft) WithElectionTimeout(d time.Duration) *Raft {
	r.electionTimeout = d
	return r
}

// ElectionTimeout returns the election timeout.
func (r *Raft) ElectionTimeout() time.Duration {
	return r.electionTimeout
}

// WithLeaderCheckInterval sets the leader check tick.
func (r *Raft) WithLeaderCheckInterval(d time.Duration) *Raft {
	r.leaderCheckInterval = d
	return r
}

// LeaderCheckInterval returns the leader check tick time.
func (r *Raft) LeaderCheckInterval() time.Duration {
	return r.leaderCheckInterval
}

// WithHeartbeatInterval sets the heartbeat tick.
func (r *Raft) WithHeartbeatInterval(d time.Duration) *Raft {
	r.heartbeatInterval = d
	return r
}

// HeartbeatInterval returns the heartbeat tick rate.
func (r *Raft) HeartbeatInterval() time.Duration {
	return r.heartbeatInterval
}

// --------------------------------------------------------------------------------
// utility methods.
// --------------------------------------------------------------------------------

// processRequestVoteResults returns the aggregate votes for in an election from rpc responses.
func (r *Raft) processRequestVoteResults(results chan *RequestVoteResults) ElectionOutcome {
	// tabulate results
	total := len(r.peers) + 1 // assume cluster size is peers + 1 (ourselves)
	resultsCount := len(results)
	votesFor := 1 // assume we voted for ourselves.

	for index := 0; index < resultsCount; index++ {
		result := <-results

		if result.Granted {
			votesFor = votesFor + 1
		}
	}

	r.debugf("election tally: %d votes for, %d total (includes self)", votesFor, total)
	return r.voteOutcome(votesFor, total)
}

// voteOutcome compares votes for to total and  it returns and integer
// indicating victory, tie, or loss. We assume both the votesFor and total
// do not include the implied self votes (you should add them before this step).
//  1 == victory
//  0 == tie
// -1 == loss
func (r *Raft) voteOutcome(votesFor, total int) ElectionOutcome {
	if total < 2 {
		return ElectionLoss
	}

	majority := total >> 1
	if total%2 == 0 {
		if votesFor > majority {
			return ElectionVictory
		} else if votesFor == majority {
			return ElectionTie
		}
		return ElectionLoss
	}

	if votesFor > majority {
		return ElectionVictory
	}
	return ElectionLoss
}

func (r *Raft) transitionTo(newState State) {
	previousState := r.State()
	if previousState != newState {
		r.debugf("transitioning to %s", newState)
	}
	r.state = newState

	switch newState {
	case Follower:
		if r.followerHandler != nil && previousState != newState {
			go r.safeExecute(r.followerHandler)
		}
	case Candidate:
		if r.candidateHandler != nil && previousState != newState {
			go r.safeExecute(r.candidateHandler)
		}
	case Leader:
		if r.leaderHandler != nil && previousState != newState {
			go r.safeExecute(r.leaderHandler)
		}
	}
}

// now returns the current time in utc.
func (r *Raft) now() time.Time {
	return time.Now().UTC()
}

// --------------------------------------------------------------------------------
// runtime methods
// --------------------------------------------------------------------------------

func (r *Raft) backoff(d time.Duration) {
	backoffTimeout := RandomTimeout(Backoff(d, r.backoffIndex))
	r.debugf("backing off for: %v", backoffTimeout)
	time.Sleep(backoffTimeout)
	atomic.AddInt32(&r.backoffIndex, 1)
}

func (r *Raft) interlocked(action func()) {
	r.Lock()
	defer r.Unlock()

	action()
}

func (r *Raft) safeExecute(action func()) {
	defer func() {
		if p := recover(); p != nil {
			r.err(fmt.Errorf("%+v", p))
		}
	}()
	action()
}

// --------------------------------------------------------------------------------
// logging methods
// --------------------------------------------------------------------------------

func (r *Raft) logErrors(errs chan error) {
	if errCount := len(errs); errCount > 0 {
		for index := 0; index < errCount; index++ {
			r.err(<-errs)
		}
	}
}

func (r *Raft) infof(format string, args ...interface{}) {
	if r.log != nil {
		r.log.SubContext("raft").SubContext(fmt.Sprintf("%v @ %d", r.State(), r.CurrentTerm())).Infof(format, args...)
	}
}

func (r *Raft) debugf(format string, args ...interface{}) {
	if r.log != nil {
		r.log.SubContext("raft").SubContext(fmt.Sprintf("%v @ %d", r.State(), r.CurrentTerm())).Debugf(format, args...)
	}
}

func (r *Raft) err(err error) {
	if r.log != nil && err != nil {
		r.log.SubContext("raft").SubContext(fmt.Sprintf("%v @ %d", r.State(), r.CurrentTerm())).Trigger(logger.Errorf(logger.Error, "%v", err))
	}
}
