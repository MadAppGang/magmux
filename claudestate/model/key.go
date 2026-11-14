package model

import (
	"cmp"
	"strconv"
)

// Key is an actor's identity: a function of ONE record's own identity fields
// (§3.4 rule 1, K1–K4). It never changes while that record keeps its identity
// fields, whatever happens to any other record. Records that produce the same
// Key merge into one session and are never re-keyed apart.
//
// Shapes:
//
//	Job != ""    => SessionID != "" && PID == 0 && Orphan == ""   (K1; K2 with a sessionId)
//	Orphan != "" => SessionID == "" && PID == 0 && Job == ""     (K4; a job record with no sessionId)
//	otherwise    => K3; PID is the record's pid after the body-else-filename
//	                fallback, and is 0 only for a record or listing entry that
//	                carries no pid
//
// An empty SessionID never joins two records. An orphan job has Job == "": its
// short id is JobFacts.Short and never part of the key.
type Key struct {
	// SessionID is "" only for a D11 record missing sessionId, and then Job == "".
	SessionID string
	PID       int
	// Job is K1's registry jobId, or K2's daemonShort else the directory name;
	// "" for an orphan job.
	Job string
	// Orphan is K4's registry file base name (never contains "/"), or
	// "jobs/" + the directory for an id-less job.
	Orphan string
}

// String spells a key as "<id>/pid:<n>", "<id>/job:<short>",
// "orphan:<file base>" (K4) or "orphan:jobs/<dir>" (an id-less job).
//
// It is injective over the shapes above: a file base name contains no "/", so
// the two orphan spellings never meet, and an orphan key has no session id.
func (k Key) String() string {
	switch {
	case k.Orphan != "":
		return "orphan:" + k.Orphan
	case k.Job != "":
		return k.SessionID + "/job:" + k.Job
	default:
		return k.SessionID + "/pid:" + strconv.Itoa(k.PID)
	}
}

// Compare orders keys by SessionID, then PID, then Job, then Orphan.
func (k Key) Compare(o Key) int {
	if c := cmp.Compare(k.SessionID, o.SessionID); c != 0 {
		return c
	}
	if c := cmp.Compare(k.PID, o.PID); c != 0 {
		return c
	}
	if c := cmp.Compare(k.Job, o.Job); c != 0 {
		return c
	}
	return cmp.Compare(k.Orphan, o.Orphan)
}
