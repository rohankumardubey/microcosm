package lib

import (
	"context"
	"sync"

	"github.com/hanfei1991/microcosm/client"
	"github.com/hanfei1991/microcosm/pkg/metadata"
	"github.com/hanfei1991/microcosm/pkg/p2p"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/stretchr/testify/mock"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type mockMasterImpl struct {
	mu sync.Mutex
	mock.Mock

	*BaseMaster
	id MasterID

	tickCount         atomic.Int64
	onlineWorkerCount atomic.Int64

	dispatchedWorkers chan WorkerHandle

	messageHandlerManager *p2p.MockMessageHandlerManager
	messageSender         p2p.MessageSender
	metaKVClient          *metadata.MetaMock
	executorClientManager *client.Manager
	serverMasterClient    *client.MockServerMasterClient
}

func newMockMasterImpl(id MasterID) *mockMasterImpl {
	ret := &mockMasterImpl{
		id:                    id,
		dispatchedWorkers:     make(chan WorkerHandle),
		messageHandlerManager: p2p.NewMockMessageHandlerManager(),
		messageSender:         p2p.NewMockMessageSender(),
		metaKVClient:          metadata.NewMetaMock(),
		executorClientManager: client.NewClientManager(),
		serverMasterClient:    &client.MockServerMasterClient{},
	}
	ret.BaseMaster = NewBaseMaster(
		ret,
		id,
		ret.messageHandlerManager,
		ret.messageSender,
		ret.metaKVClient,
		ret.executorClientManager,
		ret.serverMasterClient)

	return ret
}

func (m *mockMasterImpl) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Mock.ExpectedCalls = nil
	m.Mock.Calls = nil

	m.BaseMaster = NewBaseMaster(
		m,
		m.id,
		m.messageHandlerManager,
		m.messageSender,
		m.metaKVClient,
		m.executorClientManager,
		m.serverMasterClient)
}

func (m *mockMasterImpl) TickCount() int64 {
	return m.tickCount.Load()
}

func (m *mockMasterImpl) InitImpl(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockMasterImpl) OnMasterRecovered(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockMasterImpl) Tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tickCount.Add(1)
	log.L().Info("tick")

	args := m.Called(ctx)
	return args.Error(0)
}

func (m *mockMasterImpl) OnWorkerDispatched(worker WorkerHandle, result error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.dispatchedWorkers <- worker

	args := m.Called(worker, result)
	return args.Error(0)
}

func (m *mockMasterImpl) OnWorkerOnline(worker WorkerHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.L().Info("OnWorkerOnline", zap.Any("worker-id", worker.ID()))
	m.onlineWorkerCount.Add(1)

	args := m.Called(worker)
	return args.Error(0)
}

func (m *mockMasterImpl) OnWorkerOffline(worker WorkerHandle, reason error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.onlineWorkerCount.Sub(1)

	args := m.Called(worker, reason)
	return args.Error(0)
}

func (m *mockMasterImpl) OnWorkerMessage(worker WorkerHandle, topic p2p.Topic, message interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(worker, topic, message)
	return args.Error(0)
}

func (m *mockMasterImpl) CloseImpl(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(ctx)
	return args.Error(0)
}
