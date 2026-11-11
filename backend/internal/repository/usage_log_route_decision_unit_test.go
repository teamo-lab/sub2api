//go:build unit

package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNullRawJSONObject(t *testing.T) {
	require.False(t, nullRawJSONObject(nil).Valid)
	require.False(t, nullRawJSONObject([]byte("  ")).Valid)
	require.False(t, nullRawJSONObject([]byte(`[1,2]`)).Valid, "only objects are stored")
	require.False(t, nullRawJSONObject([]byte(`{"arm":`)).Valid, "invalid JSON becomes NULL instead of failing the insert")
	got := nullRawJSONObject([]byte(` {"arm":"B","picked":17} `))
	require.True(t, got.Valid)
	require.Equal(t, `{"arm":"B","picked":17}`, got.String)
}
