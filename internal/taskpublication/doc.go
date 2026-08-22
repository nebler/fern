// Package taskpublication publishes an already sealed and verified task result
// with repository-scoped GitHub App credentials.
//
// The package is deliberately stateless. Its caller owns durable publication
// intent and state transitions; Publisher returns only sanitized observations
// suitable for committing as reconciliation proof.
package taskpublication
