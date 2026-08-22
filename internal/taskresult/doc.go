// Package taskresult collects an immutable result tuple and canonical manifest
// from a quiesced, host-owned Git checkout. It only reads Git and the filesystem;
// callers own workspace fencing, OpenCode success proof, and durable sealing.
package taskresult
