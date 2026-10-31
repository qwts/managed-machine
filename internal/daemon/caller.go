package daemon

import (
	"path/filepath"
	"strings"
)

// Caller is the account asking to install, uninstall, or run the service.
// RegisteredHome is the account's directory home. An empty RegisteredHome
// cannot be checked and is refused.
type Caller struct {
	Username       string
	UID            int
	Home           string
	RegisteredHome string
	PrefixOwner    string
	Env            map[string]string
}

// Refuse reports why this caller may not install, uninstall, or run the
// service. The zero value is a human caller and is allowed. status is not
// gated here.
func (c Caller) Refuse() string {
	name := strings.TrimSpace(c.Username)
	lower := strings.ToLower(name)
	switch lower {
	case "root", "system", "localsystem":
		return "the daemon service is installed by the prefix owner, never as " + name
	}
	if c.UID == 0 {
		return "the daemon service is installed by the prefix owner, never as uid 0"
	}
	if agentAccount(name) || agentAccount(c.Env["AGENT_BOT_ACCOUNT"]) {
		return "agent accounts cannot install or run the daemon service"
	}
	if marker := agentEnv(c.Env); marker != "" {
		return "an agent session cannot install or run the daemon service (" + marker + ")"
	}
	home := filepath.Clean(c.Home)
	registered := filepath.Clean(c.RegisteredHome)
	if c.RegisteredHome == "" || home != registered {
		return "HOME is not the registered home of " + name
	}
	owner := strings.TrimSpace(c.PrefixOwner)
	if owner != "" && owner != name {
		return "the daemon service is installed by the prefix owner (" + owner + "), not " + name
	}
	return ""
}

func agentAccount(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	// The same glob as the legacy gate: *-*-agent.
	parts := strings.Split(name, "-")
	return len(parts) >= 3 && parts[len(parts)-1] == "agent" && parts[0] != "" && parts[1] != ""
}

func agentEnv(env map[string]string) string {
	if env["CLAUDECODE"] == "1" {
		return "CLAUDECODE"
	}
	if env["QWEN_CODE"] == "1" {
		return "QWEN_CODE"
	}
	for _, key := range []string{"CLAUDE_CODE_ENTRYPOINT", "AI_AGENT", "GH_AGENT_APP"} {
		if strings.TrimSpace(env[key]) != "" {
			return key
		}
	}
	for key, val := range env {
		if strings.HasPrefix(key, "CODEX_") {
			return key
		}
		if strings.HasSuffix(key, "_AGENT") && val == "1" && key == strings.ToUpper(key) {
			return key
		}
	}
	return ""
}
