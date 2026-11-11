package goal

import "context"

// CallKind distinguishes a fresh initial delivery, an automatic reminder, and
// ordinary or already-accepted work. It is runtime-only and never persisted.
type CallKind uint8

const (
	CallOrdinary CallKind = iota
	CallInitial
	CallFollowup
	CallContinuation
)

// CallMarker carries trusted runtime information with one agent call.
type CallMarker struct {
	Kind              CallKind
	Token             Token
	Ticket            Ticket
	Wake              Wake
	IngressGeneration uint64
}

type (
	callMarkerContextKey   struct{}
	trustedTokenContextKey struct{}
)

// WithCallMarker attaches a runtime-only goal call marker.
func WithCallMarker(ctx context.Context, marker CallMarker) context.Context {
	return context.WithValue(ctx, callMarkerContextKey{}, marker)
}

// CallMarkerFromContext returns the goal marker attached to a call.
func CallMarkerFromContext(ctx context.Context) (CallMarker, bool) {
	marker, ok := ctx.Value(callMarkerContextKey{}).(CallMarker)
	return marker, ok
}

// WithTrustedToken attaches a server-derived goal token to one model step.
func WithTrustedToken(ctx context.Context, token Token) context.Context {
	return context.WithValue(ctx, trustedTokenContextKey{}, token)
}

// TrustedTokenFromContext returns a server-derived token for a model step.
func TrustedTokenFromContext(ctx context.Context) (Token, bool) {
	token, ok := ctx.Value(trustedTokenContextKey{}).(Token)
	return token, ok
}
