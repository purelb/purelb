// Copyright 2020 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the sp
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
