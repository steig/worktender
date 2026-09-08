package main

// inboxMessage is exactly what worktender wrote to a thread file: one line,
// nothing derived from context. Which thread it belongs to is the file it
// came from, not a field here — every line in that file already says that,
// so repeating it per line would be one more thing storage and a reader could
// disagree about.
type inboxMessage struct {
	From string `json:"from"`
	Note string `json:"note"`
	At   string `json:"at"`
}

// inboxEntry is a message together with the thread it belongs to — the shape
// `inbox post --json` confirms and `inbox search --json` lists, since both
// are answering "which thread, and what was said" rather than "everything in
// one thread", which is what inboxReadJSON is for.
type inboxEntry struct {
	Thread string `json:"thread"`
	From   string `json:"from"`
	Note   string `json:"note"`
	At     string `json:"at"`
}

// inboxReadJSON is `inbox read --json`'s whole document. Messages is never
// null, even for a thread with none — jsonout.Write's consumer is a
// programmatic caller, and an empty array is one shape to branch on; null
// would be a second one for "no messages" that means the same thing.
type inboxReadJSON struct {
	Thread   string         `json:"thread"`
	Messages []inboxMessage `json:"messages"`
}

// inboxSearchJSON is `inbox search --json`'s whole document, same non-null
// rule for Entries.
type inboxSearchJSON struct {
	Query   string       `json:"query"`
	Entries []inboxEntry `json:"entries"`
}
