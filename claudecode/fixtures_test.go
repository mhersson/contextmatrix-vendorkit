package claudecode

import (
	"io/fs"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialPattern matches an Anthropic key, an OAuth token marker or an env
// dump naming the token variable.
var credentialPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}|oat01|CLAUDE_CODE_OAUTH_TOKEN=`)

func TestCredentialPatternCatchesTokenShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"api key", "x-api-key: sk-ant-" + "api03-Ab1_cD2-eF3", true},
		{"oauth token marker", `"token":"sk-` + `ant-oat01-Zz9"`, true},
		{"env dump", "CLAUDE_CODE_OAUTH_TOKEN=" + "anything", true},
		{"probe dummy bearer", "authorization: Bearer dummy-oauth", false},
		{"short prefix", "sk-ant-abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, credentialPattern.MatchString(tt.in))
		})
	}
}

// TestFixturesCarryNoToken fails if any fixture looks like a real credential.
func TestFixturesCarryNoToken(t *testing.T) {
	root, err := os.OpenRoot("testdata")
	require.NoError(t, err)

	t.Cleanup(func() { _ = root.Close() })

	fsys := root.FS()

	var files int

	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		files++

		assert.Falsef(t, credentialPattern.Match(data), "testdata/%s matches the credential pattern", path)

		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, files, "testdata holds no files")
}
