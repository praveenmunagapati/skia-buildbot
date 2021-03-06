package shortcut2

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.skia.org/infra/go/testutils"
	"go.skia.org/infra/perf/go/ds"
	"go.skia.org/infra/perf/go/dstestutil"
)

func TestShortcut(t *testing.T) {
	testutils.MediumTest(t)
	cleanup := dstestutil.InitDatastore(t, ds.SHORTCUT)

	Init(true)
	defer Init(false)
	defer cleanup()

	// Write a shortcut.
	sh := &Shortcut{
		Keys: []string{
			"https://foo",
			"https://bar",
			"https://baz",
		},
	}
	b, err := json.Marshal(sh)
	buf := bytes.NewBuffer(b)
	id, err := Insert(buf)
	assert.NoError(t, err)
	assert.NotEqual(t, "", id)

	// Read it back, confirm it is unchanged.
	sh2, err := Get(id)
	assert.NoError(t, err)
	assert.Equal(t, sh, sh2)

	err = Write("1234", sh)
	assert.NoError(t, err)
	sh3, err := Get("1234")
	assert.NoError(t, err)
	assert.Equal(t, sh, sh3)

}
