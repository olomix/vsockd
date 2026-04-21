package allowlist

import (
	"strings"
	"testing"
)

func TestNew_RejectsEmptyList(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("expected error for empty pattern list")
	}
	if _, err := New([]string{}); err == nil {
		t.Fatal("expected error for empty pattern slice")
	}
}

func TestNew_MalformedPatterns(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    string
	}{
		{"empty string", "", "empty"},
		{"missing port", "api.example.com", "host:port"},
		{"bad port number", "api.example.com:abc", "invalid port"},
		{"port out of range low", "api.example.com:0", "invalid port"},
		{"port out of range high", "api.example.com:70000", "invalid port"},
		{"star not leading label", "foo.*.example.com:443", "only allowed as leading"},
		{"star in middle", "api*.example.com:443", "only allowed as leading"},
		{"lone star dot", "*.:443", "wildcard must be"},
		{"empty host", ":443", "host must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New([]string{tc.pattern})
			if err == nil {
				t.Fatalf("expected error for %q", tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAllow_ExactMatch(t *testing.T) {
	m, err := New([]string{"api.example.com:443"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.Allow("api.example.com", 443) {
		t.Error("exact match should pass")
	}
	if m.Allow("api.example.com", 80) {
		t.Error("port mismatch must deny")
	}
	if m.Allow("other.example.com", 443) {
		t.Error("different host must deny")
	}
}

func TestAllow_HostCaseInsensitive(t *testing.T) {
	m, err := New([]string{"API.Example.com:443"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, h := range []string{"api.example.com", "API.EXAMPLE.COM", "Api.Example.Com"} {
		if !m.Allow(h, 443) {
			t.Errorf("host %q should match case-insensitively", h)
		}
	}
}

func TestAllow_SuffixWildcard(t *testing.T) {
	m, err := New([]string{"*.example.com:443"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	allow := []string{
		"foo.example.com",
		"bar.example.com",
		"a.b.example.com",
		"deep.nested.sub.example.com",
	}
	for _, h := range allow {
		if !m.Allow(h, 443) {
			t.Errorf("suffix wildcard should allow %q", h)
		}
	}

	deny := []string{
		"example.com",           // bare apex is not a strict sub-domain
		"notexample.com",        // matches text but not the dotted suffix
		"fooexample.com",        // no leading dot
		"example.com.evil.com",  // suffix must be at the end
	}
	for _, h := range deny {
		if m.Allow(h, 443) {
			t.Errorf("suffix wildcard must deny %q", h)
		}
	}

	if m.Allow("foo.example.com", 80) {
		t.Error("suffix wildcard must respect port")
	}
}

func TestAllow_AnyHostOnPort(t *testing.T) {
	m, err := New([]string{"*:8080"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.Allow("anything.example.com", 8080) {
		t.Error("*:8080 should allow any host on port 8080")
	}
	if m.Allow("anything.example.com", 443) {
		t.Error("*:8080 must deny other ports")
	}
}

func TestAllow_UniversalWildcard(t *testing.T) {
	m, err := New([]string{"*"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		host string
		port int
	}{
		{"foo.example.com", 443},
		{"10.0.0.1", 22},
		{"localhost", 80},
	}
	for _, c := range cases {
		if !m.Allow(c.host, c.port) {
			t.Errorf("universal wildcard should allow %s:%d", c.host, c.port)
		}
	}
}

func TestAllow_MultiplePatterns_OR(t *testing.T) {
	m, err := New([]string{
		"api.stripe.com:443",
		"*.s3.amazonaws.com:443",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.Allow("api.stripe.com", 443) {
		t.Error("first pattern should match")
	}
	if !m.Allow("bucket.s3.amazonaws.com", 443) {
		t.Error("second pattern should match")
	}
	if m.Allow("api.stripe.com", 80) {
		t.Error("port mismatch on first pattern must deny")
	}
	if m.Allow("s3.amazonaws.com", 443) {
		t.Error("apex must not match *.s3.amazonaws.com")
	}
	if m.Allow("evil.com", 443) {
		t.Error("unrelated host must deny")
	}
}

func TestAllow_InvalidPortRejected(t *testing.T) {
	m, err := New([]string{"*"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.Allow("example.com", 0) {
		t.Error("port 0 must be rejected even under universal wildcard")
	}
	if m.Allow("example.com", 70000) {
		t.Error("port above 65535 must be rejected")
	}
}

func TestAllow_NilMatcherDeniesAll(t *testing.T) {
	var m *Matcher
	if m.Allow("example.com", 443) {
		t.Error("nil matcher must deny everything")
	}
}
