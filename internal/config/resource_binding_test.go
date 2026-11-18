package config

import "testing"

func TestResourceBindingTokens(t *testing.T) {
	const gateway = "gateway-test-fixture-long-enough-for-token"
	const content = "content-test-fixture-long-enough-for-token"
	t.Setenv("MEDIA_GATEWAY_TOKEN", gateway)
	t.Setenv("MEDIA_USERS_TOKEN", "users-test-fixture")
	t.Setenv("MEDIA_INTERNAL_TOKEN", "")
	t.Setenv("MEDIA_ENTITIES_TOKEN", "")
	for _, invalid := range []string{gateway, "short", content + " "} {
		t.Setenv("MEDIA_CONTENT_TOKEN", invalid)
		if _, err := Load(); err == nil {
			t.Error("invalid content token accepted")
		}
	}
	t.Setenv("MEDIA_CONTENT_TOKEN", content)
	t.Setenv("MEDIA_ENTITIES_TOKEN", content)
	if _, err := Load(); err == nil {
		t.Error("shared content and entities token accepted")
	}
	t.Setenv("MEDIA_ENTITIES_TOKEN", "entities-test-fixture-long-enough-for-token")
	if _, err := Load(); err != nil {
		t.Error(err)
	}
}
