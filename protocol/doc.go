// Package protocol is the vocabulary of magmux's control socket: the event
// names, error codes and version that magmux and a client agree on.
//
// The socket is a Unix stream socket carrying line-delimited JSON, one object
// per line, each naming what it is in "type". Every connection is a
// subscriber. The first line it receives is an aggregate snapshot of every
// pane ({"type":"snapshot","panes":[...]}); after that it receives the events
// named by the Event constants as they happen. A client sends a request the
// same way, naming the verb in "type". A request carrying an "id" gets exactly
// one reply, sent only to the connection that asked:
//
//	{"type":"reply","id":7,"ok":true,"result":{...}}
//	{"type":"reply","id":7,"ok":false,"code":"no_such_pane","error":"..."}
//
// A request without an "id" is carried out and never answered. In a failed
// reply, "code" is stable and is what a caller branches on; "error" is for a
// human and may be reworded at any time. Error is that pair as a Go error,
// and CodeOf reads the code back out of any error. The `capabilities` verb
// reports Version as "protocol".
//
// It is one of magmux's two public packages, with client: the two meant for
// programs other than magmux. It imports nothing else from this module.
package protocol
