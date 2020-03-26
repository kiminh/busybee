package queue

import (
	"fmt"
	"sync"
	"testing"
	"time"

	bhmetapb "github.com/deepfabric/beehive/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/storage"
	"github.com/fagongzi/goetty"
	"github.com/fagongzi/util/protoc"
	"github.com/stretchr/testify/assert"
)

func TestConsumer(t *testing.T) {
	store, deferFunc := storage.NewTestStorage(t, true)
	defer deferFunc()

	tid := uint64(10000)
	err := store.RaftStore().AddShards(bhmetapb.Shard{
		Start:        goetty.Uint64ToBytes(tid),
		End:          goetty.Uint64ToBytes(tid + 1),
		Group:        uint64(metapb.TenantInputGroup),
		DisableSplit: true,
	})
	assert.NoError(t, err, "TestConsumer failed")

	time.Sleep(time.Second * 1)

	err = store.Set(storage.TenantMetadataKey(tid), protoc.MustMarshal(&metapb.Tenant{
		ID: tid,
		Input: metapb.TenantQueue{
			Partitions:      3,
			ConsumerTimeout: 60,
		},
	}))
	assert.NoError(t, err, "TestConsomer failed")

	err = store.PutToQueue(tid, 0, metapb.TenantInputGroup, protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   1,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("1"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   2,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("2"),
				},
			},
		},
	}), protoc.MustMarshal(&metapb.Event{
		Type: metapb.UserType,
		User: &metapb.UserEvent{
			TenantID: tid,
			UserID:   3,
			Data: []metapb.KV{
				metapb.KV{
					Key:   []byte("uid"),
					Value: []byte("3"),
				},
			},
		},
	}))
	assert.NoError(t, err, "TestConsumer failed")

	c, err := NewConsumer(tid, store, []byte("c"))
	assert.NoError(t, err, "TestConsumer failed")

	var lock sync.Mutex
	var events []metapb.UserEvent
	c.Start(3, 0, func(offset uint64, data ...[]byte) (uint64, error) {
		lock.Lock()
		defer lock.Unlock()

		for _, v := range data {
			evt := &metapb.Event{}
			protoc.MustUnmarshal(evt, v)
			events = append(events, *evt.User)
		}
		return offset, nil
	})

	time.Sleep(time.Second * 5)
	assert.Equal(t, 3, len(events), "TestConsumer failed")
	for idx, evt := range events {
		assert.Equal(t, 1, len(evt.Data), "TestConsumer failed")
		assert.Equal(t, fmt.Sprintf("%d", idx+1), string(evt.Data[0].Value), "TestConsumer failed")
	}
}
