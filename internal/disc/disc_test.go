package disc_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ilyalavrenov/swirl/internal/disc"
)

func TestTrackFileName(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		number  int
		cueType string
		want    string
	}{
		"data track":      {1, disc.TrackTypeMode1, "track01.bin"},
		"audio track":     {2, disc.TrackTypeAudio, "track02.raw"},
		"lowercase audio": {3, "audio", "track03.raw"},
		"two digits":      {10, disc.TrackTypeMode1, "track10.bin"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, test.want, disc.TrackFileName(test.number, test.cueType))
		})
	}
}

func TestIsAudio(t *testing.T) {
	t.Parallel()

	assert.True(t, disc.IsAudio(disc.TrackTypeAudio))
	assert.True(t, disc.IsAudio("audio"), "CUE keywords are case insensitive")
	assert.False(t, disc.IsAudio(disc.TrackTypeMode1))
	assert.False(t, disc.IsAudio(""))
}
