package daemon

import "testing"

func TestCallerRefuse(t *testing.T) {
	ok := Caller{Username: "admin", UID: 501, Home: "/Users/admin", RegisteredHome: "/Users/admin"}
	if msg := ok.Refuse(); msg != "" {
		t.Fatalf("owner refused: %s", msg)
	}
	cases := []struct {
		name string
		c    Caller
	}{
		{"root name", Caller{Username: "root", UID: 501, Home: "/var/root", RegisteredHome: "/var/root"}},
		{"uid 0", Caller{Username: "admin", UID: 0, Home: "/Users/admin", RegisteredHome: "/Users/admin"}},
		{"system", Caller{Username: "SYSTEM", UID: 18, Home: `C:\Windows`, RegisteredHome: `C:\Windows`}},
		{"agent account", Caller{Username: "qwts-cursor-agent", UID: 502, Home: "/Users/qwts-cursor-agent", RegisteredHome: "/Users/qwts-cursor-agent"}},
		{"agent env", Caller{Username: "admin", UID: 501, Home: "/Users/admin", RegisteredHome: "/Users/admin", Env: map[string]string{"CURSOR_AGENT": "1"}}},
		{"foreign home", Caller{Username: "admin", UID: 501, Home: "/tmp/elsewhere", RegisteredHome: "/Users/admin"}},
		{"unknown home", Caller{Username: "admin", UID: 501, Home: "/Users/admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.Refuse() == "" {
				t.Fatal("expected a refusal")
			}
		})
	}
}
