package lib

import (
	"context"
	"sync"

	"github.com/pingcap/errors"

	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/stretchr/testify/mock"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/hanfei1991/microcosm/client"
	"github.com/hanfei1991/microcosm/pkg/metadata"
	"github.com/hanfei1991/microcosm/pkg/p2p"
)

type MockMasterImpl struct {
	mu sync.Mutex
	mock.Mock

	*DefaultBaseMaster
	masterID MasterID
	id       MasterID

	tickCount         atomic.Int64
	onlineWorkerCount atomic.Int64
	isFirstStartUp    atomic.Bool

	dispatchedWorkers chan WorkerHandle
	dispatchedResult  chan error

	messageHandlerManager *p2p.MockMessageHandlerManager
	messageSender         p2p.MessageSender
	metaKVClient          *metadata.MetaMock
	executorClientManager *client.Manager
	serverMasterClient    *client.MockServerMasterClient
}

func NewMockMasterImpl(masterID, id MasterID) *MockMasterImpl {
	ret := &MockMasterImpl{
		masterID:          masterID,
		id:                id,
		dispatchedWorkers: make(chan WorkerHandle),
		dispatchedResult:  make(chan error, 1),
	}
	ret.DefaultBaseMaster = MockBaseMaster(id, ret)
	ret.messageHandlerManager = ret.DefaultBaseMaster.messageHandlerManager.(*p2p.MockMessageHandlerManager)
	ret.messageSender = ret.DefaultBaseMaster.messageSender
	ret.metaKVClient = ret.DefaultBaseMaster.metaKVClient.(*metadata.MetaMock)
	ret.executorClientManager = ret.DefaultBaseMaster.executorClientManager.(*client.Manager)
	ret.serverMasterClient = ret.DefaultBaseMaster.serverMasterClient.(*client.MockServerMasterClient)

	return ret
}

func (m *MockMasterImpl) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Mock.ExpectedCalls = nil
	m.Mock.Calls = nil

	m.DefaultBaseMaster = NewBaseMaster(
		nil,
		m,
		m.id,
		m.messageHandlerManager,
		m.messageSender,
		m.metaKVClient,
		m.executorClientManager,
		m.serverMasterClient).(*DefaultBaseMaster)
}

func (m *MockMasterImpl) TickCount() int64 {
	return m.tickCount.Load()
}

func (m *MockMasterImpl) Init(ctx context.Context) error {
	isFirstStartUp, err := m.DefaultBaseMaster.Init(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	m.isFirstStartUp.Store(isFirstStartUp)

	return nil
}

func (m *MockMasterImpl) OnMasterRecovered(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(ctx)
	return args.Error(0)
}

func (m *MockMasterImpl) Poll(ctx context.Context) error {
	if err := m.DefaultBaseMaster.Poll(ctx); err != nil {
		return errors.Trace(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.tickCount.Add(1)
	log.L().Info("tick")

	return nil
}

func (m *MockMasterImpl) OnWorkerDispatched(worker WorkerHandle, result error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.dispatchedWorkers <- worker
	m.dispatchedResult <- result

	args := m.Called(worker, result)
	return args.Error(0)
}

func (m *MockMasterImpl) OnWorkerOnline(worker WorkerHandle) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.L().Info("OnWorkerOnline", zap.Any("worker-id", worker.ID()))
	m.onlineWorkerCount.Add(1)

	args := m.Called(worker)
	return args.Error(0)
}

func (m *MockMasterImpl) OnWorkerOffline(worker WorkerHandle, reason error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.onlineWorkerCount.Sub(1)

	args := m.Called(worker, reason)
	return args.Error(0)
}

func (m *MockMasterImpl) OnWorkerMessage(worker WorkerHandle, topic p2p.Topic, message interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := m.Called(worker, topic, message)
	return args.Error(0)
}

func (m *MockMasterImpl) Close(ctx context.Context) error {
	return m.DefaultBaseMaster.Close(ctx)
}

func (m *MockMasterImpl) MasterClient() *client.MockServerMasterClient {
	return m.serverMasterClient
}

type dummyStatus struct {
	Val int
}

func (m *MockMasterImpl) GetWorkerStatusExtTypeInfo() interface{} {
	return &dummyStatus{}
}
