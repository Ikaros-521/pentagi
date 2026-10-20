package docker

import (
	"strings"
	"testing"

	"pentagi/pkg/config"
)

func envValue(env []string, key string) string {
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			return strings.TrimPrefix(entry, key+"=")
		}
	}
	return ""
}

func TestWorkerSpec_ProxyEnvInjectedWhenConfigured(t *testing.T) {
	cfg := &config.Config{
		ProxyURL:     "http://host.docker.internal:10809",
		ProxyNoProxy: "scraper,pgvector,124.223.187.198",
	}

	spec, _ := WorkerSpec(cfg, "vxcontrol/kali-linux")

	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if got := envValue(spec.Env, key); got != cfg.ProxyURL {
			t.Errorf("%s = %q, want %q", key, got, cfg.ProxyURL)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if got := envValue(spec.Env, key); got != cfg.ProxyNoProxy {
			t.Errorf("%s = %q, want %q", key, got, cfg.ProxyNoProxy)
		}
	}
}

func TestWorkerSpec_ProxyEnvDefaultsToPrivateRanges(t *testing.T) {
	spec, _ := WorkerSpec(&config.Config{ProxyURL: "http://proxy:8080"}, "vxcontrol/kali-linux")

	if got := envValue(spec.Env, "NO_PROXY"); got != defaultWorkerNoProxy {
		t.Errorf("NO_PROXY = %q, want the private-range default %q", got, defaultWorkerNoProxy)
	}
	if !strings.Contains(envValue(spec.Env, "NO_PROXY"), "10.0.0.0/8") {
		t.Error("lab networks must stay out of the proxy")
	}
}

func TestWorkerSpec_NoProxyEnvWithoutProxyURL(t *testing.T) {
	spec, _ := WorkerSpec(&config.Config{}, "vxcontrol/kali-linux")

	if len(spec.Env) != 0 {
		t.Errorf("expected no env without a proxy, got %v", spec.Env)
	}

	spec, _ = WorkerSpec(nil, "vxcontrol/kali-linux")
	if len(spec.Env) != 0 {
		t.Errorf("expected no env with a nil config, got %v", spec.Env)
	}
}
