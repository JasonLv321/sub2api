package service

import (
	"context"

	"github.com/tidwall/gjson"
)

// HTTPUpstreamProfile marks HTTP upstream requests that need provider-specific
// transport policy.
type HTTPUpstreamProfile string

const (
	HTTPUpstreamProfileDefault HTTPUpstreamProfile = ""
	HTTPUpstreamProfileOpenAI  HTTPUpstreamProfile = "openai"
	HTTPUpstreamProfileGrok    HTTPUpstreamProfile = "grok"
	// HTTPUpstreamProfileOpenAIImage is the OpenAI transport policy for image
	// endpoints. It differs from HTTPUpstreamProfileOpenAI only in the response
	// header timeout: image upstreams answer after the whole image is rendered
	// (45-100s, streaming included), far past the text first-byte budget.
	HTTPUpstreamProfileOpenAIImage HTTPUpstreamProfile = "openai_image"
	// HTTPUpstreamProfileOpenAINonStream is the OpenAI transport policy for
	// non-streaming text requests. The upstream answers only after the whole
	// completion is generated, so the streaming first-byte budget does not fit.
	HTTPUpstreamProfileOpenAINonStream HTTPUpstreamProfile = "openai_nonstream"
)

// IsOpenAI reports whether the profile uses the OpenAI transport policy
// (HTTP/2 preference, proxy fallback accounting).
func (p HTTPUpstreamProfile) IsOpenAI() bool {
	return p == HTTPUpstreamProfileOpenAI || p == HTTPUpstreamProfileOpenAIImage || p == HTTPUpstreamProfileOpenAINonStream
}

// OpenAITextUpstreamProfile picks the OpenAI text transport profile from the
// body actually sent upstream, so forced-stream rewrites are honored.
func OpenAITextUpstreamProfile(body []byte) HTTPUpstreamProfile {
	if gjson.GetBytes(body, "stream").Bool() {
		return HTTPUpstreamProfileOpenAI
	}
	return HTTPUpstreamProfileOpenAINonStream
}

type httpUpstreamProfileContextKey struct{}
type httpUpstreamDisableRedirectsContextKey struct{}

// WithHTTPUpstreamProfile injects an upstream transport profile into ctx.
func WithHTTPUpstreamProfile(ctx context.Context, profile HTTPUpstreamProfile) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if profile == HTTPUpstreamProfileDefault {
		return ctx
	}
	return context.WithValue(ctx, httpUpstreamProfileContextKey{}, profile)
}

// HTTPUpstreamProfileFromContext resolves the upstream transport profile from ctx.
func HTTPUpstreamProfileFromContext(ctx context.Context) HTTPUpstreamProfile {
	if ctx == nil {
		return HTTPUpstreamProfileDefault
	}
	profile, ok := ctx.Value(httpUpstreamProfileContextKey{}).(HTTPUpstreamProfile)
	if !ok {
		return HTTPUpstreamProfileDefault
	}
	switch profile {
	case HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileOpenAIImage, HTTPUpstreamProfileOpenAINonStream, HTTPUpstreamProfileGrok:
		return profile
	default:
		return HTTPUpstreamProfileDefault
	}
}

// WithHTTPUpstreamRedirectsDisabled prevents credential-bearing probes from
// following redirects through the shared upstream client.
func WithHTTPUpstreamRedirectsDisabled(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, httpUpstreamDisableRedirectsContextKey{}, true)
}

func HTTPUpstreamRedirectsDisabled(ctx context.Context) bool {
	return ctx != nil && ctx.Value(httpUpstreamDisableRedirectsContextKey{}) == true
}
