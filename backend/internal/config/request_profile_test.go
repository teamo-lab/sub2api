package config

import (
	"github.com/spf13/viper"
	"strings"
	"testing"
)

func TestRequestProfilingConfigEnvironment(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	setDefaults()
	if viper.GetBool("gateway.request_profiling_enabled") {
		t.Fatal("profiling must be explicitly enabled")
	}
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	t.Setenv("GATEWAY_REQUEST_PROFILING_ENABLED", "true")
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Gateway.RequestProfilingEnabled {
		t.Fatal("environment enable ignored")
	}
}

func TestProfileGroupIDsReachEnvironment(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	setDefaults()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
	t.Setenv("GATEWAY_REQUEST_PROFILING_GROUP_IDS", "38,3")
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Gateway.RequestProfilingGroupIDs) != 2 || cfg.Gateway.RequestProfilingGroupIDs[0] != 38 {
		t.Fatal(cfg.Gateway.RequestProfilingGroupIDs)
	}
}
