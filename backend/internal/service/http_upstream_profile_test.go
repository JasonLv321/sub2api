package service

import (
	"context"
	"testing"
)

func TestWithHTTPUpstreamProfile_DefaultKeepsContext(t *testing.T) {
	ctx := context.Background()
	got := WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileDefault)
	if got != ctx {
		t.Fatal("default profile should not wrap context")
	}
}

func TestWithHTTPUpstreamProfile_OpenAI(t *testing.T) {
	ctx := WithHTTPUpstreamProfile(context.TODO(), HTTPUpstreamProfileOpenAI)
	if profile := HTTPUpstreamProfileFromContext(ctx); profile != HTTPUpstreamProfileOpenAI {
		t.Fatalf("expected profile %q, got %q", HTTPUpstreamProfileOpenAI, profile)
	}
}

func TestOpenAITextUpstreamProfile(t *testing.T) {
	cases := map[string]HTTPUpstreamProfile{
		`{"model":"m","stream":true}`:  HTTPUpstreamProfileOpenAI,
		`{"model":"m","stream":false}`: HTTPUpstreamProfileOpenAINonStream,
		`{"model":"m"}`:                HTTPUpstreamProfileOpenAINonStream,
	}
	for body, want := range cases {
		if got := OpenAITextUpstreamProfile([]byte(body)); got != want {
			t.Fatalf("body %s: expected %q, got %q", body, want, got)
		}
	}
	ctx := WithHTTPUpstreamProfile(context.TODO(), HTTPUpstreamProfileOpenAINonStream)
	if profile := HTTPUpstreamProfileFromContext(ctx); profile != HTTPUpstreamProfileOpenAINonStream {
		t.Fatalf("nonstream profile must survive the context round trip, got %q", profile)
	}
	if !HTTPUpstreamProfileOpenAINonStream.IsOpenAI() {
		t.Fatal("nonstream profile keeps the OpenAI transport policy")
	}
}

func TestWithHTTPUpstreamRedirectsDisabled(t *testing.T) {
	//nolint:staticcheck // Exercises the defensive nil-context fallback.
	ctx := WithHTTPUpstreamRedirectsDisabled(nil)
	if !HTTPUpstreamRedirectsDisabled(ctx) {
		t.Fatal("expected redirects to be disabled")
	}
	if HTTPUpstreamRedirectsDisabled(context.Background()) {
		t.Fatal("redirects should remain enabled by default")
	}
}
