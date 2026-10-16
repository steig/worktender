package main

// Labels the software-factory flow puts on GitHub issues. Shared by start
// (which refuses a ready issue without acceptance criteria) and factory (which
// pulls ready issues from a queue), so the two cannot spell them differently.
const (
	labelFactoryReady      = "factory:ready"
	labelFactoryInProgress = "factory:in-progress"
)
