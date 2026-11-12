package app

// Texts the worker's dead-letter path writes into inbox.last_error when the
// final attempt(s) ended without a recorded result. They live here, not in
// cmd/worker, because the admin tooling recognises them to warn about poison
// events; one definition keeps the writer and the reader from drifting.
const (
	// GaveUpPrefix starts last_error when deadLetterCrashed gave up: the
	// attempts ran out and the last one(s) left no result (crash, kill or
	// lease expiry). The rest of the text is "N attempts; last error: ...".
	GaveUpPrefix = "gave up after "

	// NoResultRecorded is the "last error" when no attempt ever recorded a
	// result: every attempt ended in a crash or kill. The strongest sign of a
	// poison event.
	NoResultRecorded = "none recorded (the worker crashed or was killed while processing)"

	// DecodePayloadPrefix starts last_error when the worker could not decode
	// the stored payload. The payload never changes, so a replay fails the same way.
	DecodePayloadPrefix = "decode payload: "
)
