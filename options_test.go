package bigoci_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/imgoci/bigoci"
)

func TestPushRejectsOptionsItCannotHonor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  bigoci.PushOption
	}{
		{name: "a zero part size splits a file into nothing", opt: bigoci.WithPartSize(0)},
		{name: "a negative part size splits a file into nothing", opt: bigoci.WithPartSize(-1)},
		{name: "zero workers move no parts", opt: bigoci.WithWorkers(0)},
		{name: "a negative worker count moves no parts", opt: bigoci.WithWorkers(-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)
			source := bigoci.FromFile(newFile(t, payload(payloadSize)))

			_, err := newClient(t, bigoci.WithPlainHTTP()).Push(t.Context(), reg.taggedRef(tag), source, tt.opt)

			require.Error(t, err)
			requests, _ := reg.counts()
			assert.Zero(t, requests, "unusable options must be reported before any I/O")
		})
	}
}

func TestPullRejectsOptionsItCannotHonor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opt  bigoci.PullOption
	}{
		{name: "zero workers move no parts", opt: bigoci.WithWorkers(0)},
		{name: "a negative worker count moves no parts", opt: bigoci.WithWorkers(-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reg := newRegistry(t)
			seedArtifact(t, reg)
			requestsBefore, _ := reg.counts()

			err := newClient(t, bigoci.WithPlainHTTP()).Pull(
				t.Context(),
				reg.taggedRef(tag),
				bigoci.ToFile(newPath(t, destName)),
				tt.opt,
			)

			require.Error(t, err)
			requests, _ := reg.counts()
			assert.Equal(t, requestsBefore, requests, "unusable options must be reported before any I/O")
		})
	}
}

func TestWithWorkersConfiguresBothDirections(t *testing.T) {
	t.Parallel()

	reg := newRegistry(t)
	client := newClient(t, bigoci.WithPlainHTTP())
	content := payload(payloadSize)
	dest := newPath(t, destName)

	_, err := client.Push(
		t.Context(),
		reg.taggedRef(tag),
		bigoci.FromFile(newFile(t, content)),
		bigoci.WithPartSize(testPartSize),
		bigoci.WithWorkers(1),
	)
	require.NoError(t, err)

	require.NoError(t, client.Pull(t.Context(), reg.taggedRef(tag), bigoci.ToFile(dest), bigoci.WithWorkers(1)))

	assert.Len(t, reg.artifact(t).Parts, payloadParts)
	assert.FileExists(t, dest)
	assert.Equal(t, 1, reg.peakTransfers(), "one worker moves the parts of both directions one at a time")
}

// TestNewWithDockerCredentialsOnAMachineWithNoHome pins the zero-config
// contract for the environment a scratch container has: no $DOCKER_CONFIG and
// no home directory is a machine with no configuration, which is the
// anonymous case rather than an error — a public pull must not need a home.
func TestNewWithDockerCredentialsOnAMachineWithNoHome(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	client, err := bigoci.New(bigoci.WithDockerCredentials())

	require.NoError(t, err, "a machine that cannot name its configuration has none, and none is not an error")
	require.NotNil(t, client)
}
