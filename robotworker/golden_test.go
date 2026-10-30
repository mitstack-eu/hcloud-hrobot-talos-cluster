package robotworker

// The image server's user data and every rescue session command match one golden document in each location.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The image server's TLS key, its pin and the harness's port are drawn afresh on every install, so they are masked.
var (
	goldenPEMRe = regexp.MustCompile(`(?s)-----BEGIN (CERTIFICATE|PRIVATE KEY)-----.*?-----END (?:CERTIFICATE|PRIVATE KEY)-----`)
	goldenPinRe = regexp.MustCompile(`sha256//[A-Za-z0-9+/]{43}=`)
)

func TestInstall_RendersTheGoldenUserDataAndRescueCommandsInEachLocation(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "install.golden"))
	require.NoError(t, err)
	for _, dc := range []string{"FSN1-DC14", "NBG1-DC3", "HEL1-DC1"} {
		t.Run(dc, func(t *testing.T) {
			h := newHarness(t, dc)
			require.NoError(t, h.install(t))
			require.Len(t, h.cloud.created, 1)

			var got strings.Builder
			got.WriteString("== image server user data ==\n" + h.cloud.created[0].UserData)
			for i, sent := range h.rescue.sent {
				got.WriteString("\n== rescue session " + strconv.Itoa(i/2+1) + map[bool]string{true: " command ==\n", false: " stdin ==\n"}[i%2 == 0] + sent)
			}

			assert.Equal(t, string(want), goldenMask(got.String()))
		})
	}
}

func goldenMask(s string) string {
	s = goldenPEMRe.ReplaceAllStringFunc(s, func(pem string) string {
		return "<" + strings.ToLower(goldenPEMRe.FindStringSubmatch(pem)[1]) + ">"
	})
	s = goldenPinRe.ReplaceAllString(s, "<pin>")
	// Quotes and spaces never occur in the base64 of the gzipped configuration, so only the port itself is masked.
	return regexp.MustCompile(`(["\s])`+strconv.Itoa(servePort)+`(["\s])`).ReplaceAllString(s, "$1<port>$2")
}
