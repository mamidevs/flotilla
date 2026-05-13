package dispatcher

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestHintFromHTTP_HeaderPin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	r.Header.Set(HintHeaderNode, "tr-1")
	r.Header.Set(HintHeaderTags, "region:tr,scraper:a")
	r.Header.Set(HintHeaderClient, "scraper-3")

	h := HintFromHTTP(r)
	if h.PinName != "tr-1" {
		t.Errorf("PinName: %q", h.PinName)
	}
	if !reflect.DeepEqual(h.Tags, []string{"region:tr", "scraper:a"}) {
		t.Errorf("Tags: %v", h.Tags)
	}
	if h.ClientID != "scraper-3" {
		t.Errorf("ClientID: %q", h.ClientID)
	}

	// Hint headers must be stripped before forwarding.
	for _, hdr := range []string{HintHeaderNode, HintHeaderTags, HintHeaderClient} {
		if got := r.Header.Get(hdr); got != "" {
			t.Errorf("%s should be stripped, got %q", hdr, got)
		}
	}
}

func TestHintFromHTTP_FallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	r.RemoteAddr = "10.0.0.42:12345"
	h := HintFromHTTP(r)
	if h.ClientID != "10.0.0.42" {
		t.Errorf("ClientID from RemoteAddr: %q", h.ClientID)
	}
}

func TestHintFromSocksUser_KeyValue(t *testing.T) {
	known := func(name string) bool { return name == "tr-1" }

	type tc struct {
		user     string
		pin      string
		tags     []string
		clientID string
	}
	for _, c := range []tc{
		{user: "node=tr-1", pin: "tr-1"},
		{user: "tags=region:tr,scraper:b", tags: []string{"region:tr", "scraper:b"}},
		{user: "client=foo", clientID: "foo"},
		{user: "node=tr-1;tags=region:tr;client=foo", pin: "tr-1", tags: []string{"region:tr"}, clientID: "foo"},
		{user: "tr-1", pin: "tr-1"}, // bare name → pin if known
		{user: "tr-7", pin: ""},     // bare name unknown → ignored
	} {
		h := HintFromSocksUser(c.user, known)
		if h.PinName != c.pin {
			t.Errorf("user=%q: pin %q want %q", c.user, h.PinName, c.pin)
		}
		if !reflect.DeepEqual(h.Tags, c.tags) {
			if !(len(h.Tags) == 0 && len(c.tags) == 0) {
				t.Errorf("user=%q: tags %v want %v", c.user, h.Tags, c.tags)
			}
		}
		if h.ClientID != c.clientID {
			t.Errorf("user=%q: client %q want %q", c.user, h.ClientID, c.clientID)
		}
	}
}
