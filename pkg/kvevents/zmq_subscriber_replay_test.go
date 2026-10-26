/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kvevents //nolint:testpackage // tests drive unexported replay state

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
)

func TestZMQSubscriber_ReplayFailurePreservesLiveOnlyState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hasBase bool
	}{
		{name: "live-only"},
		{name: "established-base", hasBase: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool, index, _ := newTestPool(t, 16)
			pool.adapter = &sourceEndpointAdapter{}
			pool.concurrency = 1
			defer pool.Shutdown(ctx)
			z := newZMQSubscriber(pool, "pod-1", "10.0.0.1:8000", "", "", "", "kv@", false)
			z.addTask(ctx, "kv@", 5, []byte{100})
			pool.processRawMessage(ctx, drainOne(t, pool))
			key, err := index.GetRequestKey(ctx, kvblock.BlockHash(100))
			require.NoError(t, err)
			z.hasLastSeq = tc.hasBase
			if tc.hasBase {
				z.lastSeq = 5
			}

			z.invalidateReplay("kv@")
			for pool.queues[0].Len() > 0 {
				pool.processRawMessage(ctx, drainOne(t, pool))
			}

			hits, err := index.Lookup(ctx, []kvblock.BlockHash{key}, nil)
			require.NoError(t, err)
			if tc.hasBase {
				assert.Empty(t, hits[key], "failed gap recovery must invalidate the base")
			} else {
				assert.Len(t, hits[key], 1, "an empty replay failure must preserve live-only state")
			}
			assert.False(t, z.hasLastSeq)
			assert.Zero(t, z.lastSeq)
			assert.False(t, z.canAttemptReplay(), "failure must start the replay cooldown")
		})
	}
}

func TestZMQSubscriber_ColdReplayReplacesLiveOnlyState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, index, _ := newTestPool(t, 16)
	pool.adapter = &sourceEndpointAdapter{}
	pool.concurrency = 1
	defer pool.Shutdown(ctx)
	z := newZMQSubscriber(pool, "pod-1", "10.0.0.1:8000", "", "", "", "kv@", false)
	z.addTask(ctx, "kv@", 5, []byte{100})
	pool.processRawMessage(ctx, drainOne(t, pool))
	key, err := index.GetRequestKey(ctx, kvblock.BlockHash(100))
	require.NoError(t, err)

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	z.replayEndpoint = "tcp://" + listener.Addr().String()
	require.NoError(t, listener.Close())
	router := zmq4.NewRouter(ctx)
	require.NoError(t, router.Listen(z.replayEndpoint))
	defer router.Close()
	sequence := make([]byte, 8)
	binary.BigEndian.PutUint64(sequence, 5)
	replied := make(chan error, 1)
	go func() {
		msg, recvErr := router.Recv()
		if recvErr != nil {
			replied <- recvErr
			return
		}
		clientID := msg.Frames[0]
		sendErr := router.Send(zmq4.NewMsgFrom(clientID, []byte{}, []byte("kv@"), sequence, []byte{100}))
		if sendErr == nil {
			sendErr = router.Send(zmq4.NewMsgFrom(clientID, []byte{}, []byte{}, sequence, []byte{}))
		}
		replied <- sendErr
	}()

	require.True(t, z.requestReplay(ctx, 0))
	require.NoError(t, <-replied)
	for pool.queues[0].Len() > 0 {
		pool.processRawMessage(ctx, drainOne(t, pool))
	}
	require.True(t, z.hasLastSeq)
	require.Equal(t, uint64(5), z.lastSeq)
	hits, err := index.Lookup(ctx, []kvblock.BlockHash{key}, nil)
	require.NoError(t, err)
	require.Len(t, hits[key], 1, "cold replay must restore the live block")

	pool.processEventBatch(ctx, &EventBatch{
		Events: []GenericEvent{&BlockRemovedEvent{BlockHashes: []uint64{100}}},
	}, z.sourceEndpoint, "test-model")
	hits, err = index.Lookup(ctx, []kvblock.BlockHash{key}, nil)
	require.NoError(t, err)
	assert.Empty(t, hits[key], "one remove must evict a store received through both live and replay")
}
