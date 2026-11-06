package setup

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"agentq/internal/backend"
	"agentq/internal/proc"
)

type engineReport struct {
	OSVersion string `json:"os_version"`
	System    string `json:"system"`
	Arch      string `json:"architecture"`
	Git       string `json:"git"`
	backend.Probe
}

func probeCommand(home, path string, args ...string) ([]byte, error) {
	return probeCommandEnv(home, path, InstallEnv(home), args...)
}

func probeCommandEnv(home, path string, env []string, args ...string) ([]byte, error) {
	return backend.ProbeCommand(context.Background(), home, path, env, args...)
}

func InstallEnv(home string) []string {
	var env []string
	for _, s := range proc.ChildEnv() {
		k, _, _ := strings.Cut(s, "=")
		if k != "HOME" && k != "CLAUDE_CONFIG_DIR" {
			env = append(env, s)
		}
	}
	return append(env, "HOME="+home)
}

func ProbeEngine(home, explicit string) (engineReport, error) {
	return ProbeEngineEnv(home, explicit, InstallEnv(home))
}

func ProbeEngineEnv(home, explicit string, env []string) (engineReport, error) {
	return ProbeBackendEnv("", home, explicit, env)
}

// ProbeBackendEnv 查系统和 git，再让后端查 CLI。
func ProbeBackendEnv(backendName, home, explicit string, env []string) (engineReport, error) {
	r := engineReport{System: runtime.GOOS, Arch: runtime.GOARCH}
	probe := func(path string, args ...string) ([]byte, error) {
		return probeCommandEnv(home, path, env, args...)
	}
	if e := probeOS(&r, runtime.GOARCH, probe); e != nil {
		return r, e
	}
	out, e := probe("git", "--version")
	if e != nil {
		return r, errors.New(gitMissingHint)
	}
	r.Git = strings.TrimSpace(string(out))
	be := backend.Get(backendName, backend.Options{Home: home})
	r.Probe, e = be.Probe(context.Background(), backend.ProbeEnv{Home: home, Explicit: explicit, Env: env})
	return r, e
}
