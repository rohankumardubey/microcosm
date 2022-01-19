package srvdiscovery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hanfei1991/microcosm/pkg/adapter"
	"github.com/hanfei1991/microcosm/pkg/errors"
	"github.com/hanfei1991/microcosm/test"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/stretchr/testify/require"
)

func init() {
	// initialized the logger to make genEmbedEtcdConfig working.
	err := log.InitLogger(&log.Config{})
	if err != nil {
		panic(err)
	}
}

func TestEtcdDiscoveryAPI(t *testing.T) {
	t.Parallel()

	keyAdapter := adapter.ExecutorInfoKeyAdapter
	ctx, cancel := context.WithCancel(context.Background())
	_, _, client, cleanFn := test.PrepareEtcd(t, "discovery-test1")
	defer cleanFn()

	initSrvs := []struct {
		uuid string
		addr string
	}{
		{"uuid-1", "127.0.0.1:10001"},
		{"uuid-2", "127.0.0.1:10002"},
		{"uuid-3", "127.0.0.1:10003"},
	}

	updateSrvs := []struct {
		del  bool
		uuid string
		addr string
	}{
		{true, "uuid-1", "127.0.0.1:10001"},
		{false, "uuid-4", "127.0.0.1:10004"},
		{false, "uuid-5", "127.0.0.1:10005"},
	}

	for _, srv := range initSrvs {
		key := keyAdapter.Encode(srv.uuid)
		value, err := json.Marshal(&ServiceResource{Addr: srv.addr})
		require.Nil(t, err)
		_, err = client.Put(ctx, key, string(value))
		require.Nil(t, err)
	}
	tickDur := 50 * time.Millisecond
	d := NewEtcdSrvDiscovery(client, keyAdapter, tickDur)
	snapshot, err := d.Snapshot(ctx)
	require.Nil(t, err)
	require.Equal(t, 3, len(snapshot))
	require.Contains(t, snapshot, "uuid-1")

	for _, srv := range updateSrvs {
		key := keyAdapter.Encode(srv.uuid)
		value, err := json.Marshal(&ServiceResource{Addr: srv.addr})
		require.Nil(t, err)
		if srv.del {
			_, err = client.Delete(ctx, key)
			require.Nil(t, err)
		} else {
			_, err = client.Put(ctx, key, string(value))
			require.Nil(t, err)
		}
	}

	// test watch of service discovery
	ch := d.Watch(ctx)
	select {
	case wresp := <-ch:
		require.Nil(t, wresp.Err)
		require.Equal(t, 2, len(wresp.AddSet))
		require.Contains(t, wresp.AddSet, "uuid-4")
		require.Contains(t, wresp.AddSet, "uuid-5")
		require.Equal(t, 1, len(wresp.DelSet))
		require.Contains(t, wresp.DelSet, "uuid-1")
	case <-time.After(time.Second):
		require.Fail(t, "watch from service discovery timeout")
	}

	// test watch chan doesn't return when there is no change
	time.Sleep(2 * tickDur)
	select {
	case <-ch:
		require.Fail(t, "should not receive from channel when there is no change")
	default:
	}

	// test cancel will trigger watch to return an error
	cancel()
	wresp := <-ch
	require.Error(t, wresp.Err, context.Canceled.Error())

	// test duplicate watch from service discovery
	ch = d.Watch(ctx)
	wresp = <-ch
	require.Error(t, wresp.Err, errors.ErrDiscoveryDuplicateWatch.GetMsg())
}

func TestSnapshotClone(t *testing.T) {
	t.Parallel()
	snapshot := map[UUID]ServiceResource{
		"uuid-1": {Addr: "127.0.0.1:10001"},
		"uuid-2": {Addr: "127.0.0.1:10002"},
	}
	discovery := EtcdSrvDiscovery{
		snapshot:    snapshot,
		snapshotRev: 100,
	}
	cloned, rev := discovery.SnapshotClone()
	require.Equal(t, int64(100), rev)
	require.Equal(t, snapshot, cloned)
	snapshot = nil
	require.Equal(t, map[UUID]ServiceResource{
		"uuid-1": {Addr: "127.0.0.1:10001"},
		"uuid-2": {Addr: "127.0.0.1:10002"},
	}, cloned)
}
