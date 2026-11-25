package mask

import "testing"

func TestMask(t *testing.T) {
	for in, want := range map[string]string{
		"GITHUB_TOKEN=ghp_x1 go test":                         "GITHUB_TOKEN=*** go test",
		`curl -H "api_key: s3cr3tvalue99" x`:                  `curl -H "api_key: ***" x`,
		"tool --api-key hunter22 run":                         "tool --api-key *** run",
		"git clone https://bob:pa55w0rd@example.com/r.git":    "git clone https://***@example.com/r.git",
		"Bearer eyJhbGciOiJIUzI1NiJ9xQ7kLm3Np8Rt2Vw5Yz0Ab4Cd": "Bearer ***",
		// Not secrets: prose, a commit, a package path, a short value after a colon.
		"Key: run the suite":                                "Key: run the suite",
		"commit 3349138dabc50948cdc4d0cdc99879c774a78b42":   "commit 3349138dabc50948cdc4d0cdc99879c774a78b42",
		"ok  github.com/a-fig/agent-box/internal/mode 0.5s": "ok  github.com/a-fig/agent-box/internal/mode 0.5s",
		"the keyboard: fine":                                "the keyboard: fine",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}
