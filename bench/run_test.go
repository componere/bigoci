package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "no arguments", args: nil, wantCode: exitUsage, wantErr: "Commands:"},
		{name: "unknown command", args: []string{"race"}, wantCode: exitUsage, wantErr: "unknown command"},
		{name: "run without flags", args: []string{"run"}, wantCode: exitUsage, wantErr: "-spec and -out"},
		{name: "summarize without flags", args: []string{"summarize"}, wantCode: exitUsage, wantErr: "-in is required"},
		{name: "help", args: []string{"help"}, wantCode: exitOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder
			code := run(context.Background(), tt.args, &stdout, &stderr)

			assert.Equal(t, tt.wantCode, code)
			if tt.wantErr != "" {
				assert.Contains(t, stdout.String()+stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestApplyEndpoints(t *testing.T) {
	t.Parallel()

	spec := &Spec{Targets: []Target{{Name: "zot", Endpoint: "PLACEHOLDER"}}}

	require.NoError(t, applyEndpoints(spec, endpointFlag{"zot": "10.0.0.2:5000"}))
	assert.Equal(t, "10.0.0.2:5000", spec.Targets[0].Endpoint)

	err := applyEndpoints(spec, endpointFlag{"typo": "10.0.0.2:5000"})
	require.Error(t, err, "an override naming no spec target must stop the run")
	assert.Contains(t, err.Error(), "typo")
}

func TestEndpointFlagParsing(t *testing.T) {
	t.Parallel()

	flag := endpointFlag{}
	require.NoError(t, flag.Set("zot=10.0.0.2:5000"))
	require.NoError(t, flag.Set("dist=10.0.0.2:5001"))
	assert.Equal(t, "10.0.0.2:5000", flag["zot"])

	require.Error(t, flag.Set("no-equals-sign"))
	require.Error(t, flag.Set("=host"))
	require.Error(t, flag.Set("name="))
}

func TestCredentialsFromEnvRequiresBothHalves(t *testing.T) {
	t.Setenv("UNITAUTH_USERNAME", "user")
	t.Setenv("UNITAUTH_TOKEN", "")

	_, _, err := credentialsFromEnv("UNITAUTH")
	require.Error(t, err, "a half-configured credential must stop the run before it burns server time")

	t.Setenv("UNITAUTH_TOKEN", "secret")
	username, token, err := credentialsFromEnv("UNITAUTH")
	require.NoError(t, err)
	assert.Equal(t, "user", username)
	assert.Equal(t, "secret", token)
}
