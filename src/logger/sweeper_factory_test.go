package logger

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFileSweeperFactory
func TestFileSweeperFactory(t *testing.T) {
	ois := make([]OptionItem, 0)
	ois = append(ois, OptionItem{"work_dir", "/tmp"})
	ois = append(ois, OptionItem{"duration", 2})

	_, err := FileSweeperFactory(ois...)
	require.Nil(t, err)
}

// TestFileSweeperFactoryErr
func TestFileSweeperFactoryErr(t *testing.T) {
	ois := make([]OptionItem, 0)
	ois = append(ois, OptionItem{"duration", 2})

	_, err := FileSweeperFactory(ois...)
	require.NotNil(t, err)
}

// TestDBSweeperFactory
