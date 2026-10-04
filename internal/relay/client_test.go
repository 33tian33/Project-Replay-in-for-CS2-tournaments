package relay

import "testing"

func TestIPRelayAddresses(t *testing.T) {
	for raw, want := range map[string]string{
		"203.0.113.10":              "http://203.0.113.10:7790",
		" 203.0.113.10:8800 ":       "http://203.0.113.10:8800",
		"2001:db8::1":               "http://[2001:db8::1]:7790",
		"[2001:db8::1]:8800":        "http://[2001:db8::1]:8800",
		"http://203.0.113.10:7790/": "http://203.0.113.10:7790",
		"https://relay.example.com": "https://relay.example.com",
	} {
		got := NormalizeURL(raw)
		if got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", raw, got, want)
		}
		if err := ValidateURL(got); err != nil {
			t.Errorf("%q: %v", got, err)
		}
	}
	for _, raw := range []string{"http://203.0.113.10:0", "http://203.0.113.10:65536", "ftp://203.0.113.10", "http://user:pass@203.0.113.10", "http://203.0.113.10/path", "http://203.0.113.10?token=secret", "http://203.0.113.10#fragment", "http://:7790"} {
		if ValidateURL(NormalizeURL(raw)) == nil {
			t.Errorf("accepted invalid address %q", raw)
		}
	}
}
