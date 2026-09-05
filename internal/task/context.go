package task

import (
	"context"
	"errors"
)

var ErrUnauthenticated = errors.New("authenticated actor is unavailable")

type actorContextKey struct{}

// WithActor installs an identity already authenticated by ingress middleware.
func WithActor(ctx context.Context, actor ActorSnapshot) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ContextActor returns only an ingress-authenticated, structurally valid actor.
func ContextActor(ctx context.Context) (ActorSnapshot, error) {
	actor, ok := ctx.Value(actorContextKey{}).(ActorSnapshot)
	if !ok || actor.Validate() != nil {
		return ActorSnapshot{}, ErrUnauthenticated
	}
	return actor, nil
}
