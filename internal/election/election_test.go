package election

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

var nodes []string = []string{"test-node0", "test-node1", "test-node2"}

func TestWinner(t *testing.T) {
	assert.Equal(t, "test-node0", election("test-key", nodes)[0])
	assert.Equal(t, "test-node1", election("test-key-nodeXX", nodes)[0])
	assert.Equal(t, "test-node2", election("test-key-foo", nodes)[0])
}
