package raft

// AppendEntriesHandler is a delegate that handles appendn entries rpc calls.
type AppendEntriesHandler func(*AppendEntries, *AppendEntriesResults) error

// RequestVoteHandler is a delegate that handles request vote rpc calls.
type RequestVoteHandler func(*RequestVote, *RequestVoteResults) error

// Server is an raft rpc server.
type Server interface {
	// Start should bind whatever is listening for requests.
	Start() error
	// Stop should stop the server and clean up any unmanaged resources.
	Stop() error

	// SetAppendEntriesHAndler should register the append entries handler.
	SetAppendEntriesHandler(handler AppendEntriesHandler)
	// AppendEntriesHAndler returns the append entries handler.
	AppendEntriesHandler() AppendEntriesHandler

	// SetRequestVoteHandler should register the request vote handler.
	SetRequestVoteHandler(handler RequestVoteHandler)
	// RequestVoteHandler returns the request vote handler.
	RequestVoteHandler() RequestVoteHandler
}

// Client is the interface raft peers should implement.
type Client interface {
	// Open should establish the connection with the client.
	Open() error
	// Close should stop the client, and clean up any unmanaged resources.
	Close() error
	// RemoteAddr should be the identifier for the connection.
	RemoteAddr() string
	// AppendEntries should send an append entries rpc.
	AppendEntries(*AppendEntries) (*AppendEntriesResults, error)
	// RequestVote should send a request vote rpc.
	RequestVote(*RequestVote) (*RequestVoteResults, error)
}
