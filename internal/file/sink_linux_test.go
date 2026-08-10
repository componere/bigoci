//go:build linux

package file_test

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/componere/bigoci/internal/file"
)

// foreignPartialEnv carries the destination path from the root test process
// to the unprivileged helper process.
const foreignPartialEnv = "BIGOCI_FOREIGN_PARTIAL_DEST"

// TestCreateSinkRefusesAForeignOwnedPartial runs the sink under a different
// real UID from the planted partial. It requires root to prepare and launch
// the two identities, so developer runs skip it and the Linux container gate
// executes it on every security acceptance pass.
func TestCreateSinkRefusesAForeignOwnedPartial(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("launching the cross-UID helper requires root; run the Linux container gate")
	}

	dir, err := os.MkdirTemp("", "bigoci-crossuid-partial-")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })

	dest := filepath.Join(dir, "pulled.bin")
	partial := dest + file.PartialSuffix
	const (
		victimUID   = 1001
		attackerUID = 1002
		original    = "attacker-controlled bytes"
	)

	// The shared non-sticky directory is the exact environment in which the
	// attacker-owned partial was exploitable, so let both fixture UIDs use it.
	require.NoError(t, os.Chmod(dir, 0o777))
	require.NoError(t, os.WriteFile(partial, []byte(original), 0o666))
	require.NoError(t, os.Chmod(partial, 0o666))
	require.NoError(t, os.Chown(partial, attackerUID, attackerUID))

	executable, err := os.Executable()
	require.NoError(t, err)
	executable = copyExecutableForUID(t, executable)

	cmd := exec.Command(executable, "-test.run=^TestCreateSinkForeignOwnerHelper$")
	cmd.Env = append(os.Environ(), foreignPartialEnv+"="+dest)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: victimUID, Gid: victimUID},
	}

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "cross-UID helper failed:\n%s", output)
	requireFileContent(t, partial, original)
	requireAbsent(t, dest, "refusing a foreign partial must not publish a destination")

	info, statErr := os.Stat(partial)
	require.NoError(t, statErr)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "Linux file metadata must expose its UID")
	assert.Equal(t, uint32(attackerUID), stat.Uid, "a refused partial must never be chowned")
	assert.Equal(t, os.FileMode(0o666), info.Mode().Perm(), "a refused partial must never be chmodded")
}

// TestCreateSinkForeignOwnerHelper executes CreateSink as the unprivileged
// victim identity selected by TestCreateSinkRefusesAForeignOwnedPartial.
func TestCreateSinkForeignOwnerHelper(t *testing.T) {
	dest := os.Getenv(foreignPartialEnv)
	if dest == "" {
		t.Skip("the cross-UID parent did not request helper mode")
	}

	sink, err := file.CreateSink(dest)

	require.ErrorContains(t, err, "owned by user 1002, current effective user is 1001")
	assert.Nil(t, sink)
}

// copyExecutableForUID copies source into a traversable temporary directory
// so a helper running as another UID can execute the same test binary. Go's
// build directory is private to the user that invoked go test.
func copyExecutableForUID(t *testing.T, source string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "bigoci-crossuid-helper-")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })
	require.NoError(t, os.Chmod(dir, 0o755))

	target := filepath.Join(dir, "file.test")
	in, err := os.Open(source)
	require.NoError(t, err)

	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	require.NoError(t, err)

	_, copyErr := io.Copy(out, in)
	require.NoError(t, copyErr)
	require.NoError(t, out.Close())
	require.NoError(t, in.Close())
	require.NoError(t, os.Chmod(target, 0o755))

	return target
}
