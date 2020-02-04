package notify

import (
	"testing"
	"time"

	hbmetapb "github.com/deepfabric/beehive/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/metapb"
	"github.com/deepfabric/busybee/pkg/pb/rpcpb"
	"github.com/deepfabric/busybee/pkg/queue"
	"github.com/deepfabric/busybee/pkg/storage"
	"github.com/stretchr/testify/assert"
)

func TestNotify(t *testing.T) {
	s, deferFunc := storage.NewTestStorage(t, true)
	defer deferFunc()

	tenantID := uint64(1)
	assert.NoError(t, s.RaftStore().AddShards(hbmetapb.Shard{
		Group: uint64(metapb.TenantOutputGroup),
		Start: queue.PartitionKey(tenantID, 0),
		End:   queue.PartitionKey(tenantID, 1),
	}), "TestNotify failed")

	time.Sleep(time.Second * 2)

	req := rpcpb.AcquireQueueFetchRequest()
	req.Key = queue.PartitionKey(tenantID, 0)
	req.AfterOffset = 0
	req.Consumer = []byte("c")
	req.Count = 1

	_, err := s.ExecCommandWithGroup(req, metapb.TenantOutputGroup)
	assert.NoError(t, err, "TestNotify failed")

	n := NewQueueBasedNotifier(s)
	assert.NoError(t, n.Notify(tenantID, metapb.Notify{
		UserID: 1,
	}), "TestNotify failed")
}