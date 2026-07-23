package model

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

func TestUserQuotaMutationGateSerializesSameUserAcrossNodes(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	config := userQuotaMutationGateConfig{
		lease:        time.Second,
		waitTimeout:  2 * time.Second,
		retryMinimum: time.Millisecond,
		retryMaximum: 5 * time.Millisecond,
	}
	gateA := newUserQuotaMutationGate(clientA, config)
	gateB := newUserQuotaMutationGate(clientB, config)

	var active int32
	var maximum int32
	work := func() error {
		current := atomic.AddInt32(&active, 1)
		for {
			observed := atomic.LoadInt32(&maximum)
			if current <= observed || atomic.CompareAndSwapInt32(&maximum, observed, current) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		gate := gateA
		if i%2 == 1 {
			gate = gateB
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, gate.Do(context.Background(), 474, work))
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), maximum)
}

func TestUserQuotaMutationGateAllowsDifferentUsersInParallel(t *testing.T) {
	gate := newUserQuotaMutationGate(nil, userQuotaMutationGateConfig{})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	var wg sync.WaitGroup
	for _, userID := range []int{474, 475} {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			require.NoError(t, gate.Do(context.Background(), id, func() error {
				entered <- struct{}{}
				<-release
				return nil
			}))
		}(userID)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("different users were serialized behind the same lock")
		}
	}
	close(release)
	wg.Wait()
}

func TestUserQuotaMutationGateFallsBackToLocalSerialization(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	gate := newUserQuotaMutationGate(client, userQuotaMutationGateConfig{
		lease:        50 * time.Millisecond,
		waitTimeout:  20 * time.Millisecond,
		retryMinimum: time.Millisecond,
		retryMaximum: 2 * time.Millisecond,
	})

	var active int32
	var maximum int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, gate.Do(context.Background(), 474, func() error {
				current := atomic.AddInt32(&active, 1)
				for {
					observed := atomic.LoadInt32(&maximum)
					if current <= observed || atomic.CompareAndSwapInt32(&maximum, observed, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			}))
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), maximum)
}
